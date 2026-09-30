package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/buildscript"
	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/moustache"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/recipefile"
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

// exitCannotRun is returned when the test never started: an unresolvable
// version, a recipe that will not parse, a sandbox that cannot be made.
//
// It is separate from 1 because the FIRST sweep this command ever ran could
// not tell them apart. Fourteen of forty projects came back "FAIL", and four
// of those had never executed a line of their test block:
//
//	aomedia.googlesource.com/aom  GET …/+refs: 503 Service Unavailable
//	curl.se/ca-certs              no candidate version matched
//
// A 503 is not a broken package, and reporting it as one is the failure mode
// this repository keeps writing down: an answer that could not be obtained
// must not read as a negative one. Wired into the factory — which is what
// go-pkgx/bk#162 is deciding — that conflation would file bugs against
// packages for an outage.
const exitCannotRun = 4

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
		return exitCannotRun
	}
	rec, err := pantry.Parse(data)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCannotRun
	}
	tgt, err := target.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCannotRun
	}
	// Absolute, for the same reason runBuild does it: the script runs under a
	// sanitized PATH of /usr/bin:/bin:/usr/sbin:/sbin, which excludes the two
	// directories pkgx is normally installed into.
	pkgxBin := *pkgx
	if abs, err := lookPath(pkgxBin); err == nil {
		pkgxBin = abs
	}

	state, _ := runRecipeTest(testRequest{
		Recipe: rec, RecipeDir: filepath.Dir(*recipe), Project: project,
		Version: *version, Target: tgt, Sandbox: *sandbox, PkgxBin: pkgxBin,
	}, stdout, stderr)
	return state.exit()
}

// testState is what a run of a recipe's test came to.
//
// Four, not two, and named rather than numbered so a caller inside the
// process does not decode an exit status to find out which. See
// exitCannotRun for what the fourth cost before it existed.
type testState string

const (
	testPassed  testState = "pass"
	testFailed  testState = "fail"
	testNoBlock testState = "no-test"
	testNotRun  testState = "not-run"
)

// exit maps a state to the process exit status `bk test` answers with.
func (s testState) exit() int {
	switch s {
	case testPassed:
		return 0
	case testFailed:
		return 1
	case testNoBlock:
		return exitNoTest
	default:
		return exitCannotRun
	}
}

// testRequest is one package's test, decided by the caller rather than by
// flags, so `bk factory` can ask for it with the recipe it already holds.
type testRequest struct {
	Recipe    *pantry.Recipe
	RecipeDir string // holds the fixtures the test names by bare relative name
	Project   string
	Version   string // exact; empty resolves the latest the recipe offers
	Target    target.Target
	Sandbox   string // empty computes the testbed
	PkgxBin   string
	// Run executes the generated script; nil uses the package default, which
	// is the same pure-Go interpreter a build runs under.
	Run func(scriptPath string, env []string) error
}

// runRecipeTest renders a recipe's test: block and runs it in a fresh sandbox
// against the INSTALLED package.
//
// The field has existed since the recipe type was written and nothing read
// it: `grep -rn '\.Test\b'` found only toolsurface.go, which scans it for
// tool NAMES and never runs a line of it. So every `test:` block in the
// pantry was parsed and discarded, and the answer to "does this package
// work?" was nobody's (go-pkgx/bk#162).
//
// What it costs is not hypothetical. github.com/rcedgar/muscle 5.3 shipped
// for darwin/aarch64 as a binary the kernel kills on sight, and its recipe's
// first test line is `(muscle --version 2>&1 || true) | grep '{{version.raw}}'`
// — a killed process writes nothing, grep finds nothing, and the package's
// own acceptance test catches exactly the defect that reached the registry.
//
// The error it returns accompanies a state; it is never the whole answer. A
// caller that reads only the error cannot tell testNoBlock from testPassed.
func runRecipeTest(req testRequest, stdout, stderr io.Writer) (testState, error) {
	// Asked BEFORE resolving a version or touching the disk: "there is nothing
	// to run" is an answer about the recipe, and making the caller wait for a
	// version resolution to hear it would report a network fault as a missing
	// test.
	if req.Recipe.Test == nil {
		fmt.Fprintf(stdout, "%s declares no test: block — nothing was run\n", req.Project)
		return testNoBlock, nil
	}
	constraint := "*"
	if req.Version != "" {
		constraint = "=" + req.Version
	}
	ver, tag, err := versions.Resolve(req.Recipe.Versions, constraint)
	if err != nil {
		fmt.Fprintln(stderr, "error: resolve version:", err)
		return testNotRun, err
	}

	paths := config.Compute(req.Project, ver, req.Target)
	box := req.Sandbox
	if box == "" {
		box = paths.Test
	}
	// Emptied, not merely created. A test block asserts on files it wrote
	// (`test -s aln.afa`), so yesterday's sandbox can carry yesterday's
	// artefact and make today's failed run look green. "Fresh" is the word
	// the schema uses and it has to be true.
	if err := os.RemoveAll(box); err != nil {
		fmt.Fprintln(stderr, "error: clear sandbox:", err)
		return testNotRun, err
	}
	if err := os.MkdirAll(box, 0o755); err != nil {
		fmt.Fprintln(stderr, "error: create sandbox:", err)
		return testNotRun, err
	}
	if err := stageRecipeFiles(req.RecipeDir, box); err != nil {
		fmt.Fprintln(stderr, "error: stage recipe files:", err)
		return testNotRun, err
	}

	script, err := renderTest(req.Recipe, req.Project, ver, tag, req.Target, paths, box, req.RecipeDir, req.PkgxBin)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return testNotRun, err
	}
	// Beside the sandbox, never inside it: the next run empties the sandbox,
	// and a test that lists its working directory must not find our script
	// there.
	scriptPath := filepath.Join(filepath.Dir(box), filepath.Base(box)+".test.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return testNotRun, err
	}

	run := req.Run
	if run == nil {
		run = testRun
	}
	fmt.Fprintf(stdout, "test %s %s (%s/%s) in %s\n", req.Project, ver, req.Target.Platform, req.Target.Arch, box)
	if err := run(scriptPath, build.SanitizedEnv(paths.Home, config.PkgxDir())); err != nil {
		fmt.Fprintf(stderr, "FAIL %s %s: %v\n", req.Project, ver, err)
		return testFailed, err
	}
	fmt.Fprintf(stdout, "PASS %s %s\n", req.Project, ver)
	return testPassed, nil
}

// renderTest turns the recipe's test node into the runnable script.
func renderTest(rec *pantry.Recipe, project, ver, tag string, tgt target.Target,
	paths config.Paths, box, recipeDir, pkgxBin string) (string, error) {
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
		moustache.Token{From: "props", To: recipeDir},
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
		Compiler: needsCompiler(user),
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

// dirEntryInfo is os.DirEntry.Info as a seam. It fails only when the entry
// vanished between the listing and the stat — a real race against a pantry
// being updated underneath us, and one no test can provoke by arranging
// files. bk keeps such seams as package vars (build/seams.go does the same
// for os.WriteFile) rather than leaving the branch unexercised.
var dirEntryInfo = func(e os.DirEntry) (os.FileInfo, error) { return e.Info() }

// stageRecipeFiles copies the recipe directory's own files into the sandbox.
//
// Recipes name them by BARE relative name — `cc test.c -lz` in zlib.net,
// `test.pdf` in poppler, `fixture.gif` in giflib, `hello.pro` in qt.io — so a
// sandbox without them makes the test fail on the tool rather than on the
// package. Measured on pantry 2df061b: 357 recipe directories carry a file
// beside their package.yml, and the first sweep this command ever ran reported
//
//   - cc test.c -lz
//     clang: error: no such file or directory: 'test.c'
//
// which reads as a broken package and is a missing fixture.
//
// Regular files only, and no recursion: a subdirectory of a recipe directory
// is ANOTHER PROJECT (zlib.net/minizip has its own package.yml), and copying
// one into the sandbox would put a second package's sources under test.
//
// The RECIPE ITSELF is left out, and the first version of this had it wrong.
// It went in on the reasoning that excluding it "would be a rule about one
// name rather than about the directory" — and then gnu.org/coreutils failed
// the first sweep of the s390x seed:
//
//	touch test-file
//	test "$(ls -1)" = "test-file"
//
// A test that LISTS its working directory sees whatever we put there. The
// same hazard is why the generated script goes beside the sandbox rather
// than in it; the recipe is the thing that DECLARES the fixtures, not one of
// them, and recipefile.Names is where its spellings already live.
//
// The mode travels, because some of these fixtures are meant to be executed
// (agpt.co ships `entrypoint.sh`).
func stageRecipeFiles(recipeDir, sandbox string) error {
	entries, err := os.ReadDir(recipeDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if slices.Contains(recipefile.Names, e.Name()) {
			continue
		}
		info, err := dirEntryInfo(e)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(recipeDir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sandbox, e.Name()), data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// compilerNames are the command names a test uses when it compiles. Taken
// from the test surface itself rather than guessed: `bk tools --scope test
// --all` over pantry 2df061b reports cc 193, c++ 38, gcc 13, g++ 7, clang 6
// and clang++ 6 — 260 distinct recipes, not the sum of 263, because three
// call two spellings. Every name here was counted; none is a guess.
var compilerNames = map[string]bool{
	"cc": true, "c++": true, "gcc": true, "g++": true, "clang": true, "clang++": true,
}

// needsCompiler reports whether the rendered test script calls one.
//
// It PARSES, for the reason runTools already gives: a regex asked this
// question of the pantry once and reported `grep` 1231 times by counting the
// word in prose. scriptCommands returns the name in COMMAND position only,
// with shell builtins and script-defined functions subtracted, so a recipe
// whose test merely mentions gcc in a comment does not pull llvm.org in.
func needsCompiler(script string) bool {
	for _, c := range scriptCommands(script) {
		if compilerNames[c] {
			return true
		}
	}
	return false
}
