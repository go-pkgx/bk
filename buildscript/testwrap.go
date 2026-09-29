package buildscript

import (
	"fmt"
	"strings"

	"github.com/go-pkgx/bk/target"
)

// TestWrapOptions carries what a recipe's test: block needs around it.
//
// The schema calls the test "a post-build sanity test run in a fresh sandbox",
// and the sandbox is the whole point: it answers "does what we PUBLISHED
// work", which is a different question from "did the build succeed". So this
// deliberately does NOT reuse WrapOptions.
//
// What a build script has and a test script must not:
//
//	CFLAGS/LDFLAGS/rpath  a test compiles nothing. A flag set that made the
//	                      artefact link would only mask a bottle that cannot
//	                      be linked against as shipped.
//	the default llvm.org  Wrap adds a compiler when no dependency names one.
//	                      A consumer installing this package gets no compiler,
//	                      so neither may the test.
//	CMAKE_PREFIX_PATH     the whole store (go-pkgx/bk#164). A test that found
//	                      a header there would be finding it by accident.
//	$SRCROOT / the build  the sources are gone by then, and a test that reads
//	                      tree                  them is testing the tree and
//	                      not the package.
//
// What it keeps is exactly what a user has: the package itself, whatever the
// recipe declares under test.dependencies, a fresh HOME and an empty
// directory to work in.
type TestWrapOptions struct {
	UserScript string // the test node, already rendered by Generate
	// Package is the pkgspec of the package UNDER TEST, and it must name an
	// exact version. `bk test gnu.org/gawk` after building 5.4.1 with 5.3.0
	// still published would otherwise test 5.3.0 and report on the wrong
	// bytes — a green result about a build that never happened.
	Package string
	// Deps are the recipe's test.dependencies, already reduced for the target
	// and rendered as pkgspecs. They are a SEPARATE map from build and runtime
	// dependencies: a test may need a fixture generator or a diff tool that
	// the package itself must not carry.
	Deps     []string
	Home     string        // a fresh HOME, created by the script
	Sandbox  string        // the empty directory the test runs in
	PkgxDir  string        // $PKGX_DIR, so the eval resolves where the build published
	PkgxBin  string        // path to the pkgx binary
	BashPath string        // shebang interpreter (default /bin/bash)
	Host     target.Target // where we run — drives TMPDIR
}

// WrapTest renders the runnable script for a recipe's test: block.
//
// `set -x` is kept, and it matters more here than in a build: a test block is
// a list of assertions with no output of its own, so without the trace a
// failure says only that something in it returned non-zero.
func WrapTest(o TestWrapOptions) string {
	bash := o.BashPath
	if bash == "" {
		bash = "/bin/bash"
	}
	pkgxBin := o.PkgxBin
	if pkgxBin == "" {
		pkgxBin = "pkgx"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#!%s\n\nset -eo pipefail\n\n", bash)
	fmt.Fprintf(&b, "export HOME=%q\n", o.Home)
	b.WriteString("export PKGX_HOME=\"$HOME\"\n")
	b.WriteString("mkdir -p \"$HOME\"\n")
	if o.PkgxDir != "" {
		fmt.Fprintf(&b, "export PKGX_DIR=%q\n", o.PkgxDir)
	}
	b.WriteString(tmpdirLine(o.Host) + "\n")

	// ONE eval, not the build's two. The build separates the tool closure from
	// the link closure because a build tool's runtime and the artefact's link
	// requirements answer different questions (see WrapOptions.ToolDeps). A
	// test has no artefact and no link step: everything it names is something
	// that has to RUN, so splitting them would invent a distinction the recipe
	// never made.
	writeDepEval(&b, pkgxBin, o.testPlus(), "test")
	if o.PkgxBin != "" {
		fmt.Fprintf(&b, "export PKGX=%q\n", o.PkgxBin)
	}

	b.WriteString("\nset -x\n")
	fmt.Fprintf(&b, "mkdir -p %q\n", o.Sandbox)
	fmt.Fprintf(&b, "cd %q\n\n", o.Sandbox)
	b.WriteString(strings.TrimRight(o.UserScript, "\n"))
	b.WriteString("\n")
	return b.String()
}

// testPlus renders the `+spec` list: the package under test FIRST, then its
// test dependencies.
//
// First so that when a test dependency's closure drags in another version of
// the same project, the package we were asked about is the one already in the
// environment. pkgx writes each variable as VAR="new${VAR:+:$VAR}" within one
// eval in the order given, so earlier wins the head of PATH.
func (o TestWrapOptions) testPlus() string {
	parts := make([]string, 0, len(o.Deps)+1)
	if o.Package != "" {
		parts = append(parts, `"+`+o.Package+`"`)
	}
	for _, d := range o.Deps {
		parts = append(parts, `"+`+d+`"`)
	}
	return strings.Join(parts, " ")
}
