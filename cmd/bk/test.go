package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/buildscript"
	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/moustache"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bk/versions"
)

// exitNoTest is returned when the recipe declares no test: block.
//
// It is neither 0 nor 1 on purpose. A package nobody wrote a test for has not
// passed, and it has not failed either, and a caller that cannot tell the
// three apart will eventually report the first as the second. 1905 of the
// pantry's 1907 recipes carry a test block (go-pkgx/bk#162) — the two that do
// not must not come back green.
const exitNoTest = 3

// testRun executes the generated test script; a seam so a test of this file
// does not need pkgx, a registry, or the package installed.
var testRun = runBash

// runTest renders a recipe's test: block and runs it in a fresh sandbox
// against the INSTALLED package.
//
// The field has existed since the recipe type was written and nothing read
// it: `grep -rn '\.Test\b'` found only toolsurface.go, which scans it for tool
// NAMES and never runs a line of it. So every `test:` block in the pantry was
// parsed and discarded, and the answer to "does this package work?" was
// nobody's (go-pkgx/bk#162).
//
// What it costs is not hypothetical. github.com/rcedgar/muscle 5.3 shipped for
// darwin/aarch64 as a binary the kernel kills on sight, and its recipe's first
// test line is `(muscle --version 2>&1 || true) | grep '{{version.raw}}'` — a
// killed process writes nothing, grep finds nothing, and the package's own
// acceptance test catches exactly the defect that reached the registry.
//
// This runs ONE project on request. Wiring it into the factory is a separate
// decision with a cost (some tests pull network, some are slow, and a
// proportion have never run and will be wrong), and the issue lays out the
// shapes; all of them need this first.
func runTest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	recipe := fs.String("recipe", "", "path to the package.yml recipe (required)")
	version := fs.String("version", "", "exact version to test (default: latest the recipe resolves)")
	pkgx := fs.String("pkgx", "pkgx", "path to the pkgx binary used for the test env")
	sandbox := fs.String("sandbox", "", "directory to run in (default: the computed testbed)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if *recipe == "" || len(rest) < 1 {
		fmt.Fprintln(stderr, "usage: bk test --recipe <package.yml> [--version v] <project>")
		return 2
	}
	project := rest[0]

	data, err := os.ReadFile(*recipe)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rec, err := pantry.Parse(data)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// Asked BEFORE resolving a version or touching the disk: "there is nothing
	// to run" is an answer about the recipe, and making the caller wait for a
	// version resolution to hear it would report a network fault as a missing
	// test.
	if rec.Test == nil {
		fmt.Fprintf(stdout, "%s declares no test: block — nothing was run\n", project)
		return exitNoTest
	}

	tgt, err := target.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	constraint := "*"
	if *version != "" {
		constraint = "=" + *version
	}
	ver, tag, err := versions.Resolve(rec.Versions, constraint)
	if err != nil {
		fmt.Fprintln(stderr, "error: resolve version:", err)
		return 1
	}

	paths := config.Compute(project, ver, tgt)
	box := *sandbox
	if box == "" {
		box = paths.Test
	}
	// Emptied, not merely created. A test block asserts on files it wrote
	// (`test -s aln.afa`), so yesterday's sandbox can carry yesterday's
	// artefact and make today's failed run look green. "Fresh" is the word
	// the schema uses and it has to be true.
	if err := os.RemoveAll(box); err != nil {
		fmt.Fprintln(stderr, "error: clear sandbox:", err)
		return 1
	}
	if err := os.MkdirAll(box, 0o755); err != nil {
		fmt.Fprintln(stderr, "error: create sandbox:", err)
		return 1
	}

	// Absolute, for the same reason runBuild does it: the script runs under a
	// sanitized PATH of /usr/bin:/bin:/usr/sbin:/sbin, which excludes the two
	// directories pkgx is normally installed into.
	pkgxBin := *pkgx
	if abs, err := lookPath(pkgxBin); err == nil {
		pkgxBin = abs
	}

	script, err := renderTest(rec, project, ver, tag, tgt, paths, box, *recipe, pkgxBin)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// Beside the sandbox, never inside it: the next run empties the sandbox,
	// and a test that lists its working directory must not find our script
	// there.
	scriptPath := filepath.Join(filepath.Dir(box), filepath.Base(box)+".test.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	fmt.Fprintf(stdout, "test %s %s (%s/%s) in %s\n", project, ver, tgt.Platform, tgt.Arch, box)
	if err := testRun(scriptPath, build.SanitizedEnv(paths.Home, config.PkgxDir())); err != nil {
		fmt.Fprintf(stderr, "FAIL %s %s: %v\n", project, ver, err)
		return 1
	}
	fmt.Fprintf(stdout, "PASS %s %s\n", project, ver)
	return 0
}

// renderTest turns the recipe's test node into the runnable script.
func renderTest(rec *pantry.Recipe, project, ver, tag string, tgt target.Target,
	paths config.Paths, box, recipePath, pkgxBin string) (string, error) {
	// {{prefix}} is the INSTALLED prefix, not the build's staging directory.
	// That is the whole difference between this and a build: the test reads
	// the package where a consumer would find it, so a file the build made and
	// the bottle omitted is a failure here rather than a surprise later.
	toks := moustache.Prefix(paths.Install)
	toks = append(toks, moustache.Version(ver, "version")...)
	toks = append(toks, moustache.Token{From: "version.tag", To: tag})
	toks = append(toks, moustache.Host(tgt.Arch, tgt.Triple, tgt.Platform, 1)...)
	toks = append(toks,
		moustache.Token{From: "srcroot", To: box},
		moustache.Token{From: "props", To: filepath.Dir(recipePath)},
		moustache.Token{From: "pkgx.prefix", To: config.PkgxDir()},
	)
	user, err := buildscript.Generate(rec.Test, buildscript.Options{
		Target: tgt, PkgVersion: ver, Tokens: toks,
	})
	if err != nil {
		return "", fmt.Errorf("generate test script: %w", err)
	}
	return buildscript.WrapTest(buildscript.TestWrapOptions{
		UserScript: user,
		// `=ver` so depSpec renders `project@ver`: the exact bytes this run is
		// about. A bare project name would let pkgx pick whatever version the
		// registry likes, and a green line would then be about a different
		// build from the one the operator named.
		Package:  build.DepSpecs(map[string]any{project: "=" + ver}, tgt)[0],
		Deps:     testDepSpecs(rec.Test, tgt),
		Home:     paths.Home,
		Sandbox:  box,
		PkgxDir:  config.PkgxDir(),
		PkgxBin:  pkgxBin,
		BashPath: "/bin/bash",
		Host:     target.Host(),
	}), nil
}

// testDepSpecs reads test.dependencies, which is its own dep map — separate
// from the recipe's runtime and build dependencies, and platform-keyed like
// them. A test node written as a plain string or list has none.
func testDepSpecs(node any, tgt target.Target) []string {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	deps, ok := m["dependencies"].(map[string]any)
	if !ok {
		return nil
	}
	return build.DepSpecs(deps, tgt)
}
