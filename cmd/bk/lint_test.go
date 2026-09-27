package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lintTree writes a pantry checkout and returns its root.
func lintTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, "projects", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func lint(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := runLint(args, &out, &errb)
	return rc, out.String(), errb.String()
}

// Both formats, through the loader the BUILDER uses. A pantry may hold either,
// and a check that knew only one reported "the layout changed" the day our
// overlay became HCL.
func TestLintReadsBothFormats(t *testing.T) {
	dir := lintTree(t, map[string]string{
		"a.org/package.hcl":              "distributable { url = \"http://x/y.tgz\" }\n",
		"b.org/package.yml":              "distributable:\n  url: http://x/y.tgz\n",
		"x.org/protocol/xcb/package.hcl": "distributable { url = \"http://x/y.tgz\" }\n",
	})
	rc, out, errb := lint(t, "--dir", dir)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr:\n%s", rc, errb)
	}
	// The COUNT is the part that matters: a sweep that read nothing reports
	// the same absence of problems as one that read everything.
	if !strings.Contains(out, "3 recipe(s) read") {
		t.Errorf("want the count of what was read: %q", out)
	}
}

// A recipe that does not parse is named, and the run fails.
func TestLintNamesWhatItCannotRead(t *testing.T) {
	dir := lintTree(t, map[string]string{
		"good.org/package.hcl": "distributable { url = \"http://x/y.tgz\" }\n",
		"bad.org/package.hcl":  "build { script = \n",
	})
	rc, out, errb := lint(t, "--dir", dir)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if !strings.Contains(errb, "bad.org") || strings.Contains(errb, "good.org") {
		t.Errorf("the failure must name the recipe that failed, and only it:\n%s", errb)
	}
	if !strings.Contains(out, "2 recipe(s) read") {
		t.Errorf("both were read, and the count says so: %q", out)
	}
}

// An empty tree FAILS. A sweep that could not find its corpus reports the same
// "no problems" as one that read it all, and the count is the only thing that
// tells them apart — so the absence of a corpus has to be the loud case.
func TestLintRefusesAnEmptyTree(t *testing.T) {
	for name, dir := range map[string]string{
		"a projects/ with nothing in it": lintTree(t, nil),
		"no projects/ at all":            t.TempDir(),
	} {
		rc, _, errb := lint(t, "--dir", dir)
		if rc != 1 {
			t.Errorf("%s: rc = %d, want 1", name, rc)
		}
		if !strings.Contains(errb, "the layout changed") {
			t.Errorf("%s: the message must say what it means: %q", name, errb)
		}
	}
}

// A file that is not a recipe is not a recipe. The pantry holds READMEs and
// patches beside the recipes, and counting them would turn every one into a
// project with no recipe file.
func TestLintIgnoresWhatIsNotARecipe(t *testing.T) {
	dir := lintTree(t, map[string]string{
		"a.org/package.hcl": "distributable { url = \"http://x/y.tgz\" }\n",
		"a.org/README.md":   "# notes\n",
		"a.org/fix.patch":   "--- a\n+++ b\n",
	})
	if rc, out, errb := lint(t, "--dir", dir); rc != 0 || !strings.Contains(out, "1 recipe(s) read") {
		t.Errorf("rc = %d, out = %q, stderr = %q", rc, out, errb)
	}
}

func TestLintBadFlag(t *testing.T) {
	if rc, _, _ := lint(t, "--nope"); rc != 2 {
		t.Errorf("an unknown flag must be a usage error, got %d", rc)
	}
}

// A tree the walk cannot descend is an error, not an empty corpus — the same
// distinction the empty-tree case makes, from the other direction.
func TestLintUnreadableTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read an unreadable directory")
	}
	dir := lintTree(t, map[string]string{"a.org/package.hcl": "x = 1\n"})
	sub := filepath.Join(dir, "projects", "a.org")
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if rc, _, errb := lint(t, "--dir", dir); rc != 1 || !strings.Contains(errb, "lint:") {
		t.Errorf("rc = %d, stderr = %q", rc, errb)
	}
}

// A recipe sitting directly in projects/ belongs to no project. Counting it
// would name the pantry root as a project and then fail to load it.
func TestLintSkipsARecipeAtTheRoot(t *testing.T) {
	dir := lintTree(t, map[string]string{
		"package.hcl":       "distributable { url = \"http://x/y.tgz\" }\n",
		"a.org/package.hcl": "distributable { url = \"http://x/y.tgz\" }\n",
	})
	rc, out, errb := lint(t, "--dir", dir)
	if rc != 0 || !strings.Contains(out, "1 recipe(s) read") {
		t.Errorf("rc = %d, out = %q, stderr = %q", rc, out, errb)
	}
}
