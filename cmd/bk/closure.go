package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/recipefile"
	"github.com/go-pkgx/bk/target"
)

// runClosure implements `bk closure`: expand a set of pantry projects to their
// transitive runtime-dependency closure for a target platform, printed in
// TOPOLOGICAL order (deps before dependents), one per line. The factory builds a
// package's whole closure into the registry deps-first; a consumer then finds
// every dependency there too. Pure Go (no `go run ./closure` shell-out).
func runClosure(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("closure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pantryDir := fs.String("pantry", envOr("PANTRY", "pantry"), "pantry checkout dir")
	overlayDir := fs.String("overlay", envOr("PANTRY_OVERLAY_DIR", ""), "an overlay checkout consulted BEFORE the pantry, as the factory consults PKGX_PANTRY_OVERLAY. Without it this describes a build nobody performs")
	platform := fs.String("platform", envOr("PLATFORM", "linux/x86-64"), "target os/arch")
	withBuild := fs.Bool("build", false, "follow BUILD dependencies as well as runtime ones. The runtime closure is a DAG and is what a consumer needs; adding build dependencies makes it a graph with cycles, and is what FILLING an architecture from nothing actually requires")
	pins := fs.Bool("pins", false, "instead of the order, emit the `project@constraint` words a `bk factory --recipes` dispatch needs, one per line, so the version a REQUESTED project builds is the one its dependents can use. A project whose dependents cannot agree is reported as a comment rather than decided")
	constraints := fs.Bool("constraints", false, "instead of the order, list every project a dependent pins to a version line, and who asks for what. `max_versions=1` builds the newest, and the newest is not always what a dependent can use")
	implicit := fs.Bool("implicit", false, "also name the soname providers this walk cannot reach — dependencies that exist only in the compiled artefact, which no recipe declares")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	osn, arch, _ := strings.Cut(*platform, "/")
	tgt := target.Target{Platform: osn, Arch: arch}

	// The plain runtime walk keeps going through closureOf, which the factory
	// also calls: one path, so `bk closure` cannot describe an order the
	// factory would not build.
	if !*withBuild && !*constraints && !*implicit && !*pins {
		order, _ := closureOf(*pantryDir, tgt, fs.Args(), func(s string) { fmt.Fprintln(stderr, s) })
		for _, p := range order {
			fmt.Fprintln(stdout, p)
		}
		return 0
	}
	g := newClosureGraph(*pantryDir, tgt, *withBuild, func(s string) { fmt.Fprintln(stderr, s) })
	g.overlay = *overlayDir
	for _, p := range fs.Args() {
		g.visit(p)
	}
	printGraph(g, *constraints, *pins, *implicit, stdout)
	return 0
}

// closureOf expands want to its transitive runtime-dependency closure for tgt,
// in topological order (deps before dependents). Shared by `bk closure` and
// `bk factory`, which builds a closure deps-first so that a consumer of any
// package finds every one of its dependencies in the registry too.
//
// It also reports WHAT each dependent asked for. The walk reads the constraint
// off every dep spec and used to drop it on the floor, so the factory resolved
// a closure-only dependency against "*" — the newest — and the dependent's own
// build then asked for something else:
//
//	python.org 3.14.7: resolve deps: no version of bytereef.org/mpdecimal
//	satisfies "2" (available: 1)
//
// The constraint was in the spec the walk had already read.
var closureOf = func(pantryDir string, tgt target.Target, want []string, warn func(string)) ([]string, map[string][]string) {
	seen := map[string]bool{}
	var order []string
	demands := map[string][]string{}
	var visit func(proj string)
	visit = func(proj string) {
		if seen[proj] {
			return
		}
		seen[proj] = true // mark first: breaks dependency cycles
		rec, err := loadClosureRecipe(pantryDir, proj)
		if err != nil {
			// A dependency we have no recipe for can't be built by us — skip it
			// (it resolves from upstream dist at build time), but note it.
			warn(fmt.Sprintf("closure: skip %s: %v", proj, err))
			return
		}
		for dep, cons := range build.ReduceDeps(rec.Dependencies, tgt) {
			// "*" and "" say nothing, and recording them would make every
			// project look constrained.
			if cons != "" && cons != "*" && !slices.Contains(demands[dep], cons) {
				demands[dep] = append(demands[dep], cons)
			}
		}
		for _, spec := range build.DepSpecs(rec.Dependencies, tgt) {
			visit(depName(spec))
		}
		order = append(order, proj) // post-order → deps precede dependents
	}
	for _, p := range want {
		visit(p)
	}
	for _, cs := range demands {
		sort.Strings(cs)
	}
	return order, demands
}

// depName strips the version constraint from a dep spec, in BOTH the forms
// build.DepSpecs renders: "project@1.2" and "project^6"/"project>=6". Keeping
// the operator in the name made the closure look for
// projects/invisible-island.net/ncurses^6/package.yml, miss it, and drop the
// dependency — so readline was built with no ncurses in its environment.
func depName(spec string) string { return build.SpecProject(spec) }

func loadClosureRecipe(pantryDir, proj string) (*pantry.Recipe, error) {
	return recipefile.Load(pantryDir, proj)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
