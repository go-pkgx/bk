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

	blocked, err := unsatisfiable(*pantryDir, tgt, have)
	if err != nil {
		fmt.Fprintln(stderr, "depgaps:", err)
		return 1
	}
	reportGaps(stdout, blocked, *platform, *top)
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
func unsatisfiable(pantryDir string, tgt target.Target, have map[string][]string) (map[string][]string, error) {
	root := filepath.Join(pantryDir, "projects")
	out := map[string][]string{}
	err := filepathWalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "package.yml" {
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
				if c == "" {
					continue // unconstrained: any published version will do
				}
				vers, known := have[strings.ToLower(proj)]
				if !known {
					// Nothing of this project is published for this platform.
					// That is a different gap — the project itself, not a
					// version line — and mixing the two buries the actionable half.
					continue
				}
				if !anySatisfies(vers, c) {
					key := proj + " " + c
					out[key] = append(out[key], filepath.ToSlash(rel))
				}
			}
		}
		return nil
	})
	return out, err
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
