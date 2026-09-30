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
//	CFLAGS/LDFLAGS/rpath  a test may COMPILE — 260 recipes' do — but against
//	                      the package as shipped. A flag set that made the
//	                      artefact link would mask a bottle whose own headers
//	                      or rpath are wrong, which is the thing worth
//	                      catching. The compiler itself is supplied; the
//	                      build's flags are not.
//	CMAKE_PREFIX_PATH     the whole store (go-pkgx/bk#164). A test that found
//	                      a header there would be finding it by accident.
//	$SRCROOT / the build  the sources are gone by then, and a test that reads
//	                      tree                  them is testing the tree and
//	                      not the package.
//
// What it keeps is close to what a user has: the package itself, whatever the
// recipe declares under test.dependencies, the recipe's own fixture files, a
// fresh HOME and an empty directory to work in — plus a compiler when the
// test's own script calls one.
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
	Deps []string
	// Compiler asks for one in the test environment.
	//
	// The first version of this file said a test may not have a compiler,
	// because "a consumer installing this package gets none". Measured
	// against pantry 2df061b with `bk tools --scope test --all`, that is
	// wrong: 260 recipes' tests call a compiler (cc 193, c++ 38, gcc 13,
	// g++ 7, clang 6, clang++ 6 — 260 distinct, not the sum of 263, because
	// three call two of them), and only 6 declare llvm.org. A recipe's test
	// compiling a five-line program against the headers it just shipped IS
	// the convention here — zlib.net's is `cc test.c -lz` — and a sandbox
	// that refuses would have reported 260 false failures.
	//
	// Asked per test rather than always, from the rendered script's own
	// command set, so the 1500-odd tests that compile nothing do not install
	// a compiler to not use it.
	Compiler bool
	Home     string        // a fresh HOME, created by the script
	Sandbox  string        // the empty directory the test runs in
	PkgxDir  string        // $PKGX_DIR, so the eval resolves where the build published
	PkgxBin  string        // path to the pkgx binary
	BashPath string        // shebang interpreter (default /bin/bash)
	Host     target.Target // where we run — drives TMPDIR
}

// EnvFailExit is the status the test script exits with when `pkgx +…` could
// not assemble the environment — as opposed to the test itself failing.
//
// The two were one for a while, and the s390x seed's first sweep shows what
// that costs: of nine reported failures, FIVE never ran a line of their test
// block. invisible-island.net/ncurses wants github.com/tmux/tmux ^3 as a
// test dependency and no s390x tmux exists anywhere; gnu.org/readline's
// closure needs libCNS.so, which pkgx's soname map does not name. Neither is
// a bottle that does not work, and calling them failures would file bugs
// against packages for an incomplete registry.
//
// 69 is sysexits' EX_UNAVAILABLE, which is what happened. A test that exits
// 69 on its own would be read as this — and would be saying very nearly the
// same thing, which is why the collision is tolerable where an arbitrary
// number would not be.
const EnvFailExit = 69

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
	writeDepEval(&b, pkgxBin, o.testPlus(), "test", EnvFailExit)
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
	// Last, and only where the machine does not already have one: Wrap makes
	// the same exception for a darwin host, which builds with the system
	// toolchain. Appended after the recipe's own dependencies so a recipe
	// that names a compiler keeps the version it named.
	if o.Compiler && o.Host.Platform != "darwin" {
		parts = append(parts, `"+llvm.org"`)
	}
	return strings.Join(parts, " ")
}
