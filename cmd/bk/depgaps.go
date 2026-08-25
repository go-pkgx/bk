package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/overrides"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bottle"
)

// runDepgaps implements `bk depgaps`: rank the dependency constraints the
// registry cannot satisfy, by how many recipes each one blocks.
//
// A recipe whose dependency names a version LINE nobody published never
// resolves, and it says so one recipe at a time, in the middle of a factory
// run, as `resolve deps: no version of X satisfies "C"`. Counted across the
// whole pantry the same failures collapse into a short list of lines to
// publish: 310 blocked recipe-dependencies came from 154 (project, constraint)
// pairs, and the top seven accounted for 90 of them.
//
// Two details decide whether the answer is worth anything:
//
//   - --overrides must be applied first. The pantry on disk is UPSTREAM; what
//     the factory builds is the pantry plus our patches, 134 of which exist
//     solely to move openssl.org off the 1.x line. Skipping them puts a class
//     we already fixed at the top of the ranking, with 95 recipes behind it.
//   - Satisfiability is bottle's, not a local re-reading of semver. `>=1.1`
//     looks as stale as `^1.1` and is not: it is unbounded above, so openssl
//     3.x meets it and the recipe already resolves.
func runDepgaps(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("depgaps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pantryDir := fs.String("pantry", envOr("PANTRY", "pantry"), "pantry checkout to read recipes from")
	overridesDir := fs.String("overrides", "overrides", `recipe overrides to apply first ("" to skip)`)
	registry := fs.String("registry", "registry.json", `catalog of published packages: a JSON array of {name, os, arch, version}, as go-pkgx/packages' catalog tool emits`)
	platform := fs.String("platform", envOr("PLATFORM", "linux/x86-64"), "target os/arch")
	top := fs.Int("top", 30, "how many pairs to list (0 = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	osn, arch, _ := strings.Cut(*platform, "/")
	tgt := target.Target{Platform: osn, Arch: arch}

	have, err := publishedVersions(*registry, osn, arch)
	if err != nil {
		fmt.Fprintln(stderr, "depgaps:", err)
		return 1
	}
	if *overridesDir != "" {
		// A SKIPPED override is not a detail here: the ranking is a claim about
		// what the factory would face, and an override that did not apply means
		// the ranking is about a pantry the factory will not build. Apply is
		// skip-loud by design, so the warnings have to reach the operator.
		res, err := overrides.Apply(overrides.Options{
			Dir:  *overridesDir,
			Root: *pantryDir,
			Warn: func(s string) { fmt.Fprintln(stderr, s) },
		})
		if err != nil {
			fmt.Fprintln(stderr, "depgaps:", err)
			return 1
		}
		if n := len(res.Skipped); n > 0 {
			fmt.Fprintf(stderr, "depgaps: %d override(s) did NOT apply — the ranking below is off by whatever they would have fixed\n", n)
		}
	}

	blocked, absent, err := unsatisfiable(*pantryDir, tgt, have)
	if err != nil {
		fmt.Fprintln(stderr, "depgaps:", err)
		return 1
	}
	reportGaps(stdout, blocked, *platform, *top)
	// The front has two halves and the second is usually the bigger one:
	// measured on 2026-08-25, linux/x86-64 had 333 dependencies blocked by a
	// missing version LINE and 797 by a project of which nothing at all is
	// published — rust-lang.org alone accounting for 292. Reporting only the
	// first sends the operator at the smaller half.
	fmt.Fprintln(stdout)
	reportAbsent(stdout, absent, *platform, *top)
	return 0
}

// catalogRow is one published (project, platform, version) from the registry
// dump. The field names match what the catalog tool emits.
type catalogRow struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

// publishedVersions indexes the registry dump by project, for one platform.
func publishedVersions(path, osn, arch string) (map[string][]string, error) {
	b, err := osReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []catalogRow
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	have := map[string][]string{}
	for _, r := range rows {
		if r.OS == osn && r.Arch == arch {
			have[strings.ToLower(r.Name)] = append(have[strings.ToLower(r.Name)], r.Version)
		}
	}
	return have, nil
}

// unsatisfiable maps "project constraint" to the recipes that ask for it and
// cannot be served.
func unsatisfiable(pantryDir string, tgt target.Target, have map[string][]string) (out, absent map[string][]string, err error) {
	root := filepath.Join(pantryDir, "projects")
	out, absent = map[string][]string{}, map[string][]string{}
	err = filepathWalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || d.Name() != "package.yml" {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(p))
		if relErr != nil {
			return nil
		}
		data, readErr := osReadFile(p)
		if readErr != nil {
			return nil
		}
		rec, parseErr := pantry.Parse(data)
		if parseErr != nil {
			// A recipe we cannot parse is a different problem, and one the
			// schema gate already reports. Counting it here would be noise.
			return nil
		}
		for _, deps := range []map[string]any{rec.Dependencies, build.BuildDeps(rec)} {
			for _, spec := range build.DepSpecs(deps, tgt) {
				proj := build.SpecProject(spec)
				c := strings.TrimPrefix(strings.TrimPrefix(spec, proj), "@")
				vers, known := have[strings.ToLower(proj)]
				// Existence is checked BEFORE the constraint: `rust-lang.org: "*"`
				// asks for any version at all, and is blocked just as hard when
				// there is none. Skipping unconstrained deps first hid 292
				// recipes behind the one project that blocks the most.
				if !known {
					// Nothing of this project is published for this platform.
					// That is a different gap — the project itself, not a
					// version line — so it is counted separately, not mixed in.
					absent[strings.ToLower(proj)] = append(absent[strings.ToLower(proj)], filepath.ToSlash(rel))
					continue
				}
				if c == "" {
					continue // unconstrained, and something is published: fine
				}
				if !anySatisfies(vers, c) {
					key := proj + " " + c
					out[key] = append(out[key], filepath.ToSlash(rel))
				}
			}
		}
		return nil
	})
	return out, absent, err
}

// anySatisfies reports whether any published version meets the constraint,
// using bottle's semver so this agrees with what pkgx will decide at install.
func anySatisfies(vers []string, constraint string) bool {
	for _, v := range vers {
		if bottle.ParseVer(v).Satisfies(constraint) {
			return true
		}
	}
	return false
}

// reportGaps prints the ranking, worst first.
func reportGaps(w io.Writer, blocked map[string][]string, platform string, top int) {
	type entry struct {
		key      string
		blocking []string
	}
	all := make([]entry, 0, len(blocked))
	total := 0
	for k, v := range blocked {
		sort.Strings(v)
		all = append(all, entry{k, v})
		total += len(v)
	}
	sort.Slice(all, func(i, j int) bool {
		if len(all[i].blocking) != len(all[j].blocking) {
			return len(all[i].blocking) > len(all[j].blocking)
		}
		return all[i].key < all[j].key
	})
	fmt.Fprintf(w, "%d unsatisfiable (project, constraint) pair(s), blocking %d recipe-dependenc(ies) on %s\n\n",
		len(all), total, platform)
	for i, e := range all {
		if top > 0 && i == top {
			fmt.Fprintf(w, "… and %d more pair(s)\n", len(all)-top)
			break
		}
		ex := e.blocking
		if len(ex) > 3 {
			ex = ex[:3]
		}
		fmt.Fprintf(w, "%5d  %-34s  e.g. %s\n", len(e.blocking), e.key, strings.Join(ex, ", "))
	}
}

// Seams over the filesystem walk, so a test can drive the error branches a
// real directory will not reproduce.
var filepathWalkDir = filepath.WalkDir

// reportAbsent ranks the projects of which the registry carries NOTHING for
// this platform, by how many recipes depend on them.
//
// This is a different repair from a missing version line, and a cheaper one:
// every one of the twenty worst on 2026-08-25 existed upstream, so a mirror
// run closed them. It is kept apart from the version-line ranking because
// mixing them buries whichever is currently the actionable half.
func reportAbsent(w io.Writer, absent map[string][]string, platform string, top int) {
	type entry struct {
		proj string
		by   []string
	}
	all := make([]entry, 0, len(absent))
	total := 0
	for p, v := range absent {
		sort.Strings(v)
		all = append(all, entry{p, dedupeStrings(v)})
		total += len(dedupeStrings(v))
	}
	sort.Slice(all, func(i, j int) bool {
		if len(all[i].by) != len(all[j].by) {
			return len(all[i].by) > len(all[j].by)
		}
		return all[i].proj < all[j].proj
	})
	fmt.Fprintf(w, "%d project(s) with NOTHING published, blocking %d recipe-dependenc(ies) on %s\n\n",
		len(all), total, platform)
	for i, e := range all {
		if top > 0 && i == top {
			fmt.Fprintf(w, "… and %d more project(s)\n", len(all)-top)
			break
		}
		ex := e.by
		if len(ex) > 3 {
			ex = ex[:3]
		}
		fmt.Fprintf(w, "%5d  %-34s  e.g. %s\n", len(e.by), e.proj, strings.Join(ex, ", "))
	}
}

// dedupeStrings collapses a sorted slice. A recipe naming the same project in
// both its runtime and its build dependencies is one blocked recipe, not two.
func dedupeStrings(in []string) []string {
	out := in[:0:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
