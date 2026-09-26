package main

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/recipefile"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bottle"
)

// closureGraph walks a project set following runtime dependencies and,
// optionally, build dependencies too.
//
// The two are different graphs and it matters which one a question is about.
// The runtime closure is what a CONSUMER needs and it is a DAG, so the factory
// can build it deps-first. Adding build dependencies makes it a graph with
// cycles — gnu.org/gcc build-depends on gnu.org/gcc, curl.se/ca-certs runs
// curl to fetch its bundle while curl needs the bundle — and on an
// architecture with no bottles at all that is the difference between a plan
// and a wish.
//
// Every reduction goes through build.ReduceDeps, the same one the builder
// uses. A separate reading of the same YAML is a proxy, and the proxy this
// replaces was wrong three times: it missed `>=5<8.13` because its operator
// list had no ">", missed a constraint with a trailing `#` comment, and could
// not see the soname channel at all.
type closureGraph struct {
	order    []string // post-order: dependencies before dependents
	missing  []string // named as a dependency, no recipe in this pantry
	demands  map[string]map[string][]string
	seen     map[string]bool
	pantry   string
	overlay  string
	tgt      target.Target
	withBuil bool
	warn     func(string)
}

func newClosureGraph(pantryDir string, tgt target.Target, withBuild bool, warn func(string)) *closureGraph {
	return &closureGraph{
		demands: map[string]map[string][]string{},
		seen:    map[string]bool{},
		pantry:  pantryDir, tgt: tgt, withBuil: withBuild, warn: warn,
	}
}

// visit marks before recursing, which breaks cycles rather than hiding them:
// with --build the graph HAS cycles, and a walk that refused to terminate on
// one would be answering a question nobody asked.
func (g *closureGraph) visit(proj string) {
	if g.seen[proj] {
		return
	}
	g.seen[proj] = true
	rec, err := recipefile.LoadOverlay(g.overlay, g.pantry, proj)
	if err != nil {
		g.missing = append(g.missing, proj)
		if g.warn != nil {
			g.warn(fmt.Sprintf("closure: skip %s: %v", proj, err))
		}
		return
	}
	g.record(proj, rec.Dependencies)
	for _, spec := range build.DepSpecs(rec.Dependencies, g.tgt) {
		g.visit(build.SpecProject(spec))
	}
	if g.withBuil {
		bd := build.BuildDeps(rec)
		g.record(proj, bd)
		for _, spec := range build.DepSpecs(bd, g.tgt) {
			g.visit(build.SpecProject(spec))
		}
	}
	g.order = append(g.order, proj)
}

// record notes who asked for what. A constraint is kept only when it says
// something: "*" and "" are every version, and listing them would bury the
// handful that pin a LINE under a hundred that do not.
func (g *closureGraph) record(by string, deps map[string]any) {
	for dep, cons := range build.ReduceDeps(deps, g.tgt) {
		if cons == "" || cons == "*" {
			continue
		}
		if g.demands[dep] == nil {
			g.demands[dep] = map[string][]string{}
		}
		// A project can ask for the same thing twice — rust-lang.org/cargo
		// names openssl.org in BOTH its runtime and its build dependencies —
		// and "asked by cargo, cargo" reads as two demands where there is one.
		if !slices.Contains(g.demands[dep][cons], by) {
			g.demands[dep][cons] = append(g.demands[dep][cons], by)
		}
	}
}

// constrained lists, in the graph's own order, every project some dependent
// pins to a version line, with who asks for what.
//
// This is what `max_versions=1` cannot know. It builds the NEWEST, and
// openssl.org 4.0.2 does not satisfy curl.se's "^3" — one dispatch, one
// discovery, on a two-core machine.
func (g *closureGraph) constrained() []string {
	var out []string
	for _, proj := range g.order {
		d := g.demands[proj]
		if len(d) == 0 {
			continue
		}
		out = append(out, proj)
		cs := make([]string, 0, len(d))
		for c := range d {
			cs = append(cs, c)
		}
		sort.Strings(cs)
		for _, c := range cs {
			by := append([]string(nil), d[c]...)
			sort.Strings(by)
			out = append(out, fmt.Sprintf("    %-14s asked by %s", c, strings.Join(by, ", ")))
		}
	}
	return out
}

// implicitExtras are the soname providers this walk could not have reached.
//
// A dependency the maps supply exists only in the compiled artefact — perl
// declares nothing about libcrypt — so a walk over RECIPES is blind to it by
// construction, and a plan built from one discovers them a failed build at a
// time. The set is bounded, so naming it turns a surprise into a list.
//
// Reported separately from the order, and never mixed into it: these are
// projects that MAY be pulled in, not ones this closure is known to need.
func (g *closureGraph) implicitExtras() []string {
	var out []string
	for _, p := range bottle.SonameProviders() {
		if !g.seen[p] {
			out = append(out, p)
		}
	}
	return out
}

// printGraph writes whichever view the flags asked for.
func printGraph(g *closureGraph, constraints, implicit bool, stdout io.Writer) {
	if constraints {
		for _, l := range g.constrained() {
			fmt.Fprintln(stdout, l)
		}
		return
	}
	for _, p := range g.order {
		fmt.Fprintln(stdout, p)
	}
	if implicit {
		for _, p := range g.implicitExtras() {
			fmt.Fprintf(stdout, "%s\t# soname provider — no recipe declares it\n", p)
		}
	}
}
