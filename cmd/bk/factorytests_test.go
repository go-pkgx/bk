package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
)

// testFactory is a factory wired for testPublished and nothing else.
func testFactory(t *testing.T, out *bytes.Buffer, recipeDir string) *factory {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PKGX_PANTRY_DIR", dir)
	t.Setenv("PKGX_DIR", filepath.Join(dir, "pkgx"))
	return &factory{
		runner:      &build.Runner{RecipeDir: recipeDir, PkgxBin: "pkgx"},
		tgt:         target.Target{Platform: "linux", Arch: "x86-64"},
		platform:    "linux/x86-64",
		runTests:    true,
		testTimeout: time.Minute,
		testCounts:  map[testState]int{},
		stdout:      out, stderr: out,
	}
}

func parseRecipe(t *testing.T, body string) (*pantry.Recipe, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj.org")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := pantry.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return rec, dir
}

// A test outcome is RECORDED and never changes the run's result — the whole
// point of go-pkgx/bk#246. A gate would stop a chunk over a missing
// /etc/containers/policy.json.
func TestFactoryRecordsTestOutcomesWithoutFailingTheRun(t *testing.T) {
	for _, tc := range []struct {
		name, recipe string
		runErr       error
		want         testState
		wantLine     string
	}{
		{"passes", "versions:\n  - 1.2.3\nbuild: make\ntest: true\n", nil, testPassed, "TEST PASS"},
		{"fails", "versions:\n  - 1.2.3\nbuild: make\ntest: false\n", errors.New("exit 1"), testFailed, "TEST FAIL"},
		{"no test block", "versions:\n  - 1.2.3\nbuild: make\n", nil, testNoBlock, "TEST NONE"},
		{"never ran", "versions:\n  - 1.2.3\nbuild: make\ntest: true\n", nil, testNotRun, "TEST NOT-RUN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			rec, dir := parseRecipe(t, tc.recipe)
			f := testFactory(t, &out, dir)
			prev := factoryTestRunner
			factoryTestRunner = func(time.Duration, io.Writer, io.Writer) func(string, []string) error {
				return func(string, []string) error { return tc.runErr }
			}
			t.Cleanup(func() { factoryTestRunner = prev })
			if tc.want == testNotRun {
				// A version the recipe cannot resolve: the test never starts.
				f.testPublished(rec, "proj.org", "9.9.9")
			} else {
				f.testPublished(rec, "proj.org", "1.2.3")
			}

			if got := f.testCounts[tc.want]; got != 1 {
				t.Errorf("want one %s, counts = %v", tc.want, f.testCounts)
			}
			if !strings.Contains(out.String(), tc.wantLine) {
				t.Errorf("want %q in:\n%s", tc.wantLine, out.String())
			}
			if !strings.HasPrefix(f.tests.String(), string(tc.want)+" proj.org ") {
				t.Errorf("tests.txt line is wrong: %q", f.tests.String())
			}
			// None of it may touch the run's result.
			if f.ok != 0 || f.failed != 0 || f.skipped != 0 {
				t.Errorf("a test outcome changed the run: ok=%d failed=%d skipped=%d", f.ok, f.failed, f.skipped)
			}
			if f.failures.Len() != 0 {
				t.Errorf("a test outcome reached failures.txt: %q", f.failures.String())
			}
		})
	}
}

// The deadline has to reach the RUNNING SCRIPT, not merely the goroutine
// waiting on it. Exercised against the real interpreter with a script that
// sleeps, because a factory-level test cannot tell a deadline from any other
// way of failing fast — the first version of this one passed in 10ms because
// `pkgx` was not on PATH, and would have passed with no deadline at all.
func TestRunBashLimitedStopsAScriptThatWouldNotStop(t *testing.T) {
	script := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	start := time.Now()
	err := runBashLimited(80*time.Millisecond, &out, &out)(script, []string{"PATH=/usr/bin:/bin"})
	took := time.Since(start)
	if err == nil {
		t.Error("a script stopped by the deadline must report an error")
	}
	if took > 10*time.Second {
		t.Errorf("the deadline did not reach the script: took %s", took)
	}
	// And with no limit the same script is left alone — the control that
	// says the deadline, not something else, is what stopped it.
	if runBashLimited(0, &out, &out) == nil {
		t.Error("runBashLimited(0) must still return a runner")
	}
}

func TestFactorySkipsTestingWhenAskedTo(t *testing.T) {
	var out bytes.Buffer
	rec, dir := parseRecipe(t, "versions:\n  - 1.2.3\nbuild: make\ntest: true\n")
	f := testFactory(t, &out, dir)
	f.runTests = false
	f.testPublished(rec, "proj.org", "1.2.3")
	if out.Len() != 0 || f.tests.Len() != 0 {
		t.Errorf("--test=false still ran something: %q / %q", out.String(), f.tests.String())
	}
	var rep bytes.Buffer
	f.reportTests(&rep)
	if rep.Len() != 0 {
		t.Errorf("a report for tests nobody ran: %q", rep.String())
	}
}

// Printed even at zero: a silent section reads as "nothing was wrong", and
// this one has to be able to say "nothing was tried".
func TestFactoryReportsTestsEvenWithNothingToSay(t *testing.T) {
	var out, rep bytes.Buffer
	f := testFactory(t, &out, t.TempDir())
	f.reportTests(&rep)
	if !strings.Contains(rep.String(), "0 passed, 0 failed") {
		t.Errorf("want the tally at zero: %q", rep.String())
	}
}

func TestFactoryNamesTheFailingTests(t *testing.T) {
	var out, rep bytes.Buffer
	f := testFactory(t, &out, t.TempDir())
	f.testCounts[testFailed] = 1
	f.tests.WriteString("pass a.org 1 linux/x86-64\nfail b.org 2 linux/x86-64\n")
	f.reportTests(&rep)
	if !strings.Contains(rep.String(), "fail b.org 2") {
		t.Errorf("the failure is not named: %q", rep.String())
	}
	if strings.Contains(rep.String(), "pass a.org") {
		t.Errorf("a pass was listed among the failures: %q", rep.String())
	}
}

// --test-only builds nothing and tests what the registry already holds.
// The factory tests what it publishes ONCE; this is how a bottle that
// stopped working — a dependency whose signature went bad, say — gets
// noticed after the fact.
func TestFactoryTestOnlyBuildsNothing(t *testing.T) {
	var out bytes.Buffer
	rec, dir := parseRecipe(t, "versions:\n  - 1.2.3\nbuild: make\ntest: true\n")
	f := testFactory(t, &out, dir)
	f.testOnly = true

	prevHas, prevRun := factoryHasPlatform, factoryTestRunner
	factoryHasPlatform = func(string, string, string, string, string) (bool, error) { return true, nil }
	factoryTestRunner = func(time.Duration, io.Writer, io.Writer) func(string, []string) error {
		return func(string, []string) error { return nil }
	}
	built := false
	prevBuild := factoryBuild
	factoryBuild = func(*build.Runner, *pantry.Recipe, string, string, target.Target, target.Target, string) (build.Result, error) {
		built = true
		return build.Result{}, nil
	}
	t.Cleanup(func() { factoryHasPlatform, factoryTestRunner, factoryBuild = prevHas, prevRun, prevBuild })

	f.buildOne(rec, "proj.org", "1.2.3")
	if built {
		t.Error("--test-only built something")
	}
	if f.testCounts[testPassed] != 1 {
		t.Errorf("counts = %v", f.testCounts)
	}
	if f.ok != 0 || f.skipped != 0 || f.failed != 0 {
		t.Errorf("ok=%d skipped=%d failed=%d", f.ok, f.skipped, f.failed)
	}
}

// A platform with no bottle and a registry that would not answer are BOTH
// "not run", and neither is a failure — an unobtained answer must not read
// as a negative one.
func TestFactoryTestOnlyDistinguishesAbsentFromUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published bool
		err       error
		wantIn    string
	}{
		{"no bottle here", false, nil, "no bottle here"},
		{"registry unreachable", false, errors.New("dial tcp: refused"), "publish-check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			rec, dir := parseRecipe(t, "versions:\n  - 1.2.3\nbuild: make\ntest: true\n")
			f := testFactory(t, &out, dir)
			f.testOnly = true
			prev := factoryHasPlatform
			factoryHasPlatform = func(string, string, string, string, string) (bool, error) {
				return tc.published, tc.err
			}
			t.Cleanup(func() { factoryHasPlatform = prev })

			f.buildOne(rec, "proj.org", "1.2.3")
			if f.testCounts[testNotRun] != 1 {
				t.Errorf("counts = %v", f.testCounts)
			}
			if f.failed != 0 {
				t.Errorf("an unobtained answer was counted as a failure")
			}
			if !strings.Contains(out.String(), tc.wantIn) {
				t.Errorf("want %q in:\n%s", tc.wantIn, out.String())
			}
		})
	}
}

// An override that did not apply still stops --test-only: the test: block is
// part of the recipe, so one rendered from an unpatched recipe asks a
// different question than the operator thinks.
func TestFactoryTestOnlyStillHonoursASkippedOverride(t *testing.T) {
	var out bytes.Buffer
	rec, dir := parseRecipe(t, "versions:\n  - 1.2.3\nbuild: make\ntest: true\n")
	f := testFactory(t, &out, dir)
	f.testOnly = true
	f.skippedOverrides = map[string][]string{"proj.org": {"a.patch"}}
	f.buildOne(rec, "proj.org", "1.2.3")
	if f.failed != 1 {
		t.Errorf("failed = %d, want 1", f.failed)
	}
	if len(f.testCounts) != 0 {
		t.Errorf("a test was recorded for a recipe we refused: %v", f.testCounts)
	}
}
