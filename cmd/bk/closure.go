package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/pantry"
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
	platform := fs.String("platform", envOr("PLATFORM", "linux/x86-64"), "target os/arch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	osn, arch, _ := strings.Cut(*platform, "/")
	tgt := target.Target{Platform: osn, Arch: arch}

	seen := map[string]bool{}
	var order []string
	var visit func(proj string)
	visit = func(proj string) {
		if seen[proj] {
			return
		}
		seen[proj] = true // mark first: breaks dependency cycles
		rec, err := loadClosureRecipe(*pantryDir, proj)
		if err != nil {
			// A dependency we have no recipe for can't be built by us — skip it
			// (it resolves from upstream dist at build time), but note it.
			fmt.Fprintf(stderr, "closure: skip %s: %v\n", proj, err)
			return
		}
		for _, spec := range build.DepSpecs(rec.Dependencies, tgt) {
			visit(depName(spec))
		}
		order = append(order, proj) // post-order → deps precede dependents
	}
	for _, p := range fs.Args() {
		visit(p)
	}
	for _, p := range order {
		fmt.Fprintln(stdout, p)
	}
	return 0
}

// depName strips the version constraint from a "project@constraint" dep spec.
func depName(spec string) string {
	if i := strings.IndexByte(spec, '@'); i >= 0 {
		return spec[:i]
	}
	return spec
}

func loadClosureRecipe(pantryDir, proj string) (*pantry.Recipe, error) {
	b, err := os.ReadFile(filepath.Join(pantryDir, "projects", proj, "package.yml"))
	if err != nil {
		return nil, err
	}
	return pantry.Parse(b)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
