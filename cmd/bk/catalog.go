package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/logical"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bottle"
)

// runCatalog builds the catalogue a browser reads, and optionally publishes
// it.
//
// # WHY THIS EXISTS
//
// An anonymous client cannot enumerate ghcr: `/v2/_catalog` answers 403 and
// only per-project tag lists are served. So `pkgx ls` and `gnu.org/<TAB>`
// have nothing to read unless the registry carries a catalogue of its own.
// bottle#114 made it an ordinary bottle; this is what fills it.
//
// # NAMES WITHOUT A SWEEP, BY DEFAULT
//
// The pantry has ~1900 projects. Asking the registry which versions each one
// has published is ~1900 requests, and this repository has twice paid for
// treating a per-project loop as free. The default therefore reads the
// pantry and the overlay and nothing else: no network, and the catalogue
// says what projects EXIST.
//
// That is not a degraded answer for the job it has. Tab-completion needs
// NAMES; a version beside a candidate is a courtesy. `--versions` opts into
// the sweep for a run that wants it, and says how many requests it will make
// before it makes them.
func runCatalog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pantryDir := fs.String("pantry", envOr("PANTRY", "pantry"), "pantry checkout dir")
	overlayDir := fs.String("overlay", envOr("PANTRY_OVERLAY_DIR", ""), "an overlay checkout read beside the pantry")
	platform := fs.String("platform", envOr("PLATFORM", "linux/x86-64"), "the platform this catalogue is FOR")
	out := fs.String("o", "", "write the JSON here instead of stdout")
	overridesDir := fs.String("overrides", envOr("OVERRIDES", ""), "directory of *.hcl logical recipe overrides, applied as the factory applies them")
	publish := fs.Bool("publish", false, "push it to $PKGX_DIST as a bottle, for this platform")
	withVersions := fs.Bool("versions", false, "ask the registry what each project has published (one request per project)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	osn, arch, ok := strings.Cut(*platform, "/")
	if !ok {
		fmt.Fprintf(stderr, "catalog: --platform wants os/arch, not %q\n", *platform)
		return 2
	}

	projects, err := catalogProjects(*pantryDir, *overlayDir)
	if err != nil {
		fmt.Fprintln(stderr, "catalog:", err)
		return 1
	}
	if len(projects) == 0 {
		// An empty catalogue would be published and believed. A pantry that
		// read as empty is a mistyped path far more often than an empty
		// pantry, and the two must not have the same outcome.
		fmt.Fprintf(stderr, "catalog: no projects under %q — an empty catalogue is not a result\n", *pantryDir)
		return 1
	}

	cat := bottle.Catalog{Generated: catalogNow().UTC().Format(time.RFC3339)}
	lset, err := logical.LoadDir(*overridesDir)
	if err != nil {
		fmt.Fprintln(stderr, "catalog:", err)
		return 2
	}
	tgt := target.Target{Platform: osn, Arch: arch}
	var noRecipe int
	for _, p := range projects {
		e := bottle.CatalogProject{Project: p}
		// The RUNTIME dependencies, reduced for this platform, so a reader
		// can walk the tree with no pantry and no network (bottle#117).
		// Still no requests: these come off the recipe already on disk.
		if r, _, err := recipeLoader(lset, *overlayDir, *pantryDir, p); err == nil && r != nil {
			for _, spec := range build.DepSpecs(r.Dependencies, tgt) {
				if n := depName(spec); n != "" {
					e.Deps = append(e.Deps, n)
				}
			}
			sort.Strings(e.Deps)
		} else {
			// Counted, not silent. A project the walk NAMED and the reader
			// cannot load is a hole in the dependency half of the
			// catalogue, and a catalogue that hid it would show those
			// projects as having no dependencies at all — which is a
			// statement, and a false one.
			noRecipe++
		}
		cat.Projects = append(cat.Projects, e)
	}
	if noRecipe > 0 {
		fmt.Fprintf(stderr, "catalog: %d project(s) named but unreadable — listed with no dependencies\n", noRecipe)
	}
	if *withVersions {
		fmt.Fprintf(stderr, "catalog: asking the registry about %d project(s), one request each\n", len(cat.Projects))
		fillVersions(cat.Projects, osn, arch, func(s string) { fmt.Fprintln(stderr, s) })
	}

	body, err := marshalCatalog(cat)
	if err != nil {
		fmt.Fprintln(stderr, "catalog:", err)
		return 1
	}
	switch {
	case *publish:
		c, err := catalogClient(bottle.DistBase)
		if err != nil {
			fmt.Fprintln(stderr, "catalog:", err)
			return 1
		}
		if err := publishCatalog(c, cat, osn, arch); err != nil {
			fmt.Fprintln(stderr, "catalog: publish:", err)
			return 1
		}
		fmt.Fprintf(stderr, "catalog: %d project(s) → %s %s/%s\n", len(cat.Projects), bottle.DistBase, osn, arch)
	case *out != "":
		if err := osWriteFile(*out, body, 0o644); err != nil {
			fmt.Fprintln(stderr, "catalog:", err)
			return 1
		}
		fmt.Fprintf(stderr, "catalog: %d project(s) → %s\n", len(cat.Projects), *out)
	default:
		fmt.Fprint(stdout, string(body))
	}
	return 0
}

// seams, so a test needs neither a clock nor a registry.
var (
	catalogNow     = time.Now
	publishCatalog = bottle.PublishCatalog
	versionsFor    = bottle.VersionsFor
	catalogClient  = bottle.NewOCIClient
	marshalCatalog = bottle.MarshalCatalog
)

// fillVersions asks the registry what each project has published.
//
// Failures are per project and are WARNED, not fatal: a catalogue missing
// one project's versions is still a catalogue, and a registry hiccup
// two thousand requests in must not throw the other 1999 away.
func fillVersions(projects []bottle.CatalogProject, osn, arch string, warn func(string)) {
	for i := range projects {
		vs, err := versionsFor(projects[i].Project, osn, arch)
		if err != nil {
			warn(fmt.Sprintf("catalog: %s: %v", projects[i].Project, err))
			continue
		}
		if len(vs) == 0 {
			continue
		}
		var out []string
		for _, v := range vs {
			out = append(out, v.Raw)
		}
		// Newest first, which is the order a reader wants and the order the
		// completion's one-line note takes its value from.
		sort.Sort(sort.Reverse(sort.StringSlice(out)))
		projects[i].Versions = out
		projects[i].Platforms = []string{osn + "/" + arch}
	}
}

// catalogProjects is every project named by the pantry or the overlay.
//
// The UNION, because both are real: the factory builds from the pantry with
// overrides applied, and a consumer resolves from the overlay. A project in
// only one of them is still a project somebody can ask for, and a catalogue
// that read one half would complete onto a name the other half cannot build
// — or hide one it can.
func catalogProjects(pantryDir, overlayDir string) ([]string, error) {
	seen := map[string]bool{}
	for _, root := range []string{pantryDir, overlayDir} {
		if root == "" {
			continue
		}
		base := filepath.Join(root, "projects")
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			// An error AT THE ROOT is the whole walk failing and is returned;
			// one further down is a single unreadable directory and is
			// skipped. Swallowing both is how a mistyped pantry path came to
			// report "no projects" -- the message for an empty pantry -- and
			// the coverage gate is what showed it, by leaving the branch that
			// reports a read failure with nothing able to reach it.
			if err != nil {
				if p == base {
					return err
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			switch d.Name() {
			case "package.yml", "package.yaml", "package.hcl":
			default:
				return nil
			}
			// TrimPrefix rather than filepath.Rel: p is under base by
			// construction, and Rel's error arm is a branch nothing can
			// reach -- which this gate is right to refuse.
			rel := strings.TrimPrefix(filepath.Dir(p), base+string(filepath.Separator))
			seen[filepath.ToSlash(rel)] = true
			return nil
		})
		// A pantry that cannot be walked at all is reported; an overlay that
		// is simply absent is not. The first is a mistyped path, the second
		// is the ordinary case.
		if err != nil && root == pantryDir {
			return nil, err
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
