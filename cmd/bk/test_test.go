package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/target"
)

// testbed points bk's path computation at a scratch checkout, so a test never
// reads or writes the developer's real ~/.pkgx or XDG data home.
func testTestbed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PKGX_PANTRY_DIR", dir)
	t.Setenv("PKGX_DIR", filepath.Join(dir, "pkgx"))
	return dir
}

// writeTestRecipe drops a package.yml in its own project directory, as a pantry
// holds one, and returns its path.
func writeTestRecipe(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj.org")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "package.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubRun replaces the script runner and records what it was handed.
func stubRun(t *testing.T, err error) *string {
	t.Helper()
	var got string
	prev := testRun
	testRun = func(scriptPath string, _ []string) error {
		got = scriptPath
		return err
	}
	t.Cleanup(func() { testRun = prev })
	return &got
}

// requireNonRootHere skips a case whose whole mechanism is a permission
// root does not have. The repository already does this in fixup and builder;
// the CI runner is unprivileged, so the branch stays covered there.
func requireNonRootHere(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: the permission this case relies on does not apply")
	}
}

const okRecipe = "versions:\n  - 1.2.3\nbuild: make\ntest: proj --version | grep 1.2.3\n"

func TestRunTestUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no project", []string{"--recipe", "/x/package.yml"}},
		{"no recipe", []string{"proj.org"}},
		{"bad flag", []string{"--nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runTest(tc.args, &out, &errOut); code != 2 {
				t.Errorf("want 2, got %d (%s)", code, errOut.String())
			}
		})
	}
}

func TestRunTestUnreadableRecipe(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runTest([]string{"--recipe", filepath.Join(t.TempDir(), "absent.yml"), "proj.org"}, &out, &errOut)
	if code != 1 {
		t.Errorf("want 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "error:") {
		t.Errorf("no error said: %q", errOut.String())
	}
}

func TestRunTestUnparseableRecipe(t *testing.T) {
	p := writeTestRecipe(t, "versions: [1.0.0]\nbuild: make\ntest: 3\nthis is: not: yaml:\n")
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut); code != 1 {
		t.Errorf("want 1, got %d (%s)", code, errOut.String())
	}
}

// The heart of go-pkgx/bk#162: a recipe with no test block has not passed.
// Exit 3 is neither 0 nor 1 so a caller cannot collapse the three outcomes.
func TestRunTestSaysWhenThereIsNoTestBlock(t *testing.T) {
	testTestbed(t)
	p := writeTestRecipe(t, "versions:\n  - 1.2.3\nbuild: make\n")
	var out, errOut bytes.Buffer
	code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut)
	if code != exitNoTest {
		t.Errorf("want %d, got %d", exitNoTest, code)
	}
	if code == 0 || code == 1 {
		t.Error("a missing test must not read as a pass or a failure")
	}
	if !strings.Contains(out.String(), "declares no test") {
		t.Errorf("unclear message: %q", out.String())
	}
}

func TestRunTestUnresolvableTarget(t *testing.T) {
	testTestbed(t)
	t.Setenv("BREWKIT_TARGET", "not-a-target")
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut); code != 1 {
		t.Errorf("want 1, got %d", code)
	}
}

func TestRunTestUnresolvableVersion(t *testing.T) {
	testTestbed(t)
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	code := runTest([]string{"--recipe", p, "--version", "9.9.9", "proj.org"}, &out, &errOut)
	if code != 1 {
		t.Errorf("want 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "resolve version") {
		t.Errorf("want the version step named: %q", errOut.String())
	}
}

func TestRunTestPasses(t *testing.T) {
	testTestbed(t)
	got := stubRun(t, nil)
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("want 0, got %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "PASS proj.org 1.2.3") {
		t.Errorf("no PASS line: %q", out.String())
	}
	script, err := os.ReadFile(*got)
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	// The exact version, not a bare project name: a green line has to be
	// about the build the operator named.
	if !strings.Contains(s, `"+proj.org@1.2.3"`) {
		t.Errorf("the package is not pinned in the eval:\n%s", s)
	}
	if !strings.Contains(s, "proj --version | grep 1.2.3") {
		t.Errorf("the recipe's test did not reach the script:\n%s", s)
	}
}

func TestRunTestFails(t *testing.T) {
	testTestbed(t)
	stubRun(t, os.ErrPermission)
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut); code != 1 {
		t.Errorf("want 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "FAIL proj.org 1.2.3") {
		t.Errorf("no FAIL line: %q", errOut.String())
	}
}

// "Fresh sandbox" has to be true. A test block asserts on files it writes, so
// a leftover from the previous run can make a failed run look green.
func TestRunTestEmptiesTheSandbox(t *testing.T) {
	testTestbed(t)
	stubRun(t, nil)
	box := filepath.Join(t.TempDir(), "box")
	if err := os.MkdirAll(box, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(box, "yesterday.out")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "--sandbox", box, "proj.org"}, &out, &errOut); code != 0 {
		t.Fatalf("want 0, got %d (%s)", code, errOut.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the previous run's artefact survived into the sandbox")
	}
}

// Beside the sandbox, not in it: the next run empties the sandbox, and a test
// that lists its working directory must not find our script there.
func TestRunTestWritesTheScriptOutsideTheSandbox(t *testing.T) {
	testTestbed(t)
	got := stubRun(t, nil)
	box := filepath.Join(t.TempDir(), "box")
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "--sandbox", box, "proj.org"}, &out, &errOut); code != 0 {
		t.Fatalf("want 0, got %d (%s)", code, errOut.String())
	}
	if strings.HasPrefix(*got, box+string(os.PathSeparator)) {
		t.Errorf("script %q is inside the sandbox %q", *got, box)
	}
	entries, err := os.ReadDir(box)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("sandbox is not empty: %v", entries)
	}
}

func TestRunTestReportsASandboxItCannotClear(t *testing.T) {
	requireNonRootHere(t)
	testTestbed(t)
	parent := filepath.Join(t.TempDir(), "ro")
	box := filepath.Join(parent, "box")
	if err := os.MkdirAll(filepath.Join(box, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "--sandbox", box, "proj.org"}, &out, &errOut); code != 1 {
		t.Errorf("want 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "clear sandbox") {
		t.Errorf("want the step named: %q", errOut.String())
	}
}

func TestRunTestReportsASandboxItCannotCreate(t *testing.T) {
	requireNonRootHere(t)
	testTestbed(t)
	// A read-only PARENT with nothing at the sandbox path: RemoveAll returns
	// nil (there is nothing to remove) and MkdirAll cannot create it. A file
	// in the parent's place would not do — RemoveAll fails on that with
	// ENOTDIR and the "clear" arm answers first.
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	code := runTest([]string{"--recipe", p, "--sandbox", filepath.Join(parent, "box"), "proj.org"}, &out, &errOut)
	if code != 1 {
		t.Errorf("want 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "create sandbox") {
		t.Errorf("want the step named: %q", errOut.String())
	}
}

func TestRunTestReportsAScriptItCannotWrite(t *testing.T) {
	testTestbed(t)
	stubRun(t, nil)
	dir := t.TempDir()
	box := filepath.Join(dir, "box")
	// Occupy the script's path with a DIRECTORY, which os.WriteFile cannot
	// overwrite.
	if err := os.MkdirAll(filepath.Join(dir, "box.test.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "--sandbox", box, "proj.org"}, &out, &errOut); code != 1 {
		t.Errorf("want 1, got %d", code)
	}
}

func TestRunTestReportsAnUnrenderableTestBlock(t *testing.T) {
	testTestbed(t)
	// A guarded step with no `run`. The guard `>=1` matches version 1.2.3 on
	// every platform, so the step is kept and Generate then has nothing to
	// render — it refuses rather than emitting an empty command.
	p := writeTestRecipe(t, "versions:\n  - 1.2.3\nbuild: make\ntest:\n  script:\n    - if: '>=1'\n")
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut); code != 1 {
		t.Errorf("want 1, got %d (%s)", code, errOut.String())
	}
}

// test.dependencies is its own map, and platform-keyed like the others.
func TestRunTestInstallsTheRecipesTestDependencies(t *testing.T) {
	testTestbed(t)
	got := stubRun(t, nil)
	p := writeTestRecipe(t, "versions:\n  - 1.2.3\nbuild: make\n"+
		"test:\n  dependencies:\n    gnu.org/diffutils: ^3\n  script: diff a b\n")
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut); code != 0 {
		t.Fatalf("want 0, got %d (%s)", code, errOut.String())
	}
	script, err := os.ReadFile(*got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `"+gnu.org/diffutils^3"`) {
		t.Errorf("test dependency missing:\n%s", script)
	}
}

func TestTestDepSpecsIgnoresWhatIsNotADepMap(t *testing.T) {
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	if got := testDepSpecs("just a script", tgt); got != nil {
		t.Errorf("a string test block has no dependencies, got %v", got)
	}
	if got := testDepSpecs(map[string]any{"script": "x"}, tgt); got != nil {
		t.Errorf("a map without dependencies has none, got %v", got)
	}
	if got := testDepSpecs(map[string]any{"dependencies": "nonsense"}, tgt); got != nil {
		t.Errorf("a non-map dependencies value yields none, got %v", got)
	}
}

// The dispatch arm. Without it `bk test` is unreachable from the command
// line, and nothing else in the suite goes through main's switch for it.
func TestTestCommandIsDispatched(t *testing.T) {
	// No --recipe, so runTest answers 2 (usage) — which is enough to prove
	// the arm was taken, and needs no recipe, pkgx or registry to do it.
	if code, _, errOut := run2(t, "test"); code != 2 {
		t.Errorf("code=%d, stderr=%q", code, errOut)
	}
}

// lookPath is asked because the script runs under a sanitized PATH. Both
// answers are exercised HERE rather than left to the machine: pkgx is on a
// developer's PATH and not on CI's, so leaving it implicit makes this branch
// covered in one place and not the other — which is how it first arrived red.
func TestRunTestResolvesPkgxToAnAbsolutePath(t *testing.T) {
	testTestbed(t)
	got := stubRun(t, nil)
	prev := lookPath
	lookPath = func(string) (string, error) { return "/opt/elsewhere/pkgx", nil }
	t.Cleanup(func() { lookPath = prev })

	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "proj.org"}, &out, &errOut); code != 0 {
		t.Fatalf("want 0, got %d (%s)", code, errOut.String())
	}
	script, err := os.ReadFile(*got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "/opt/elsewhere/pkgx") {
		t.Errorf("the resolved pkgx did not reach the script:\n%s", script)
	}
}

func TestRunTestKeepsTheGivenPkgxWhenItCannotBeResolved(t *testing.T) {
	testTestbed(t)
	got := stubRun(t, nil)
	prev := lookPath
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = prev })

	p := writeTestRecipe(t, okRecipe)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"--recipe", p, "--pkgx", "/my/pkgx", "proj.org"}, &out, &errOut); code != 0 {
		t.Fatalf("want 0, got %d (%s)", code, errOut.String())
	}
	script, err := os.ReadFile(*got)
	if err != nil {
		t.Fatal(err)
	}
	// Kept as given: an explicit path still works and the original error
	// stays legible instead of being replaced by an empty string.
	if !strings.Contains(string(script), "/my/pkgx") {
		t.Errorf("the given pkgx was lost:\n%s", script)
	}
}
