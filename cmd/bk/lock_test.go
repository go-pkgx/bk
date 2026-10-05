package main

import (
	"bytes"

	"errors"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-pkgx/bk/logical"
	"github.com/go-pkgx/bk/pantry"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lockbed gives a pantry, a frozen clock and a captured write, so a lock's
// output is a fact a test can assert rather than a thing that changes every
// second.
func lockbed(t *testing.T, recipes map[string]string) (dir string, written *map[string][]byte) {
	t.Helper()
	dir = t.TempDir()
	for proj, body := range recipes {
		p := filepath.Join(dir, "projects", proj)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "package.yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prevNow, prevWrite := lockNow, osWriteFile
	lockNow = func() time.Time { return time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) }
	files := map[string][]byte{}
	osWriteFile = func(name string, b []byte, _ os.FileMode) error { files[name] = b; return nil }
	t.Cleanup(func() { lockNow, osWriteFile = prevNow, prevWrite })
	return dir, &files
}

const lockableRecipe = "versions:\n  - 1.2.3\n  - 1.2.4\ndistributable:\n  url: https://x/v{{version}}.tgz\nbuild:\n  script: [make]\n"

func TestLockPinsTheResolvedVersions(t *testing.T) {
	p, _ := lockbed(t, map[string]string{
		"app.org": "dependencies:\n  lib.org: '*'\n" + lockableRecipe,
		"lib.org": lockableRecipe,
	})
	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "--platform", "linux/x86-64", "app.org"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{`"app.org" = "1.2.4"`, `"lib.org" = "1.2.4"`, "linux/x86-64", "2026-10-05T09:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("the lock does not carry %q:\n%s", want, got)
		}
	}
	// Sorted by project, not in build order: a lock is read as a diff, and a
	// topological order makes every line move when one dependency does.
	if strings.Index(got, `"app.org"`) > strings.Index(got, `"lib.org"`) {
		t.Error("the lock is in build order, not sorted")
	}
	// It says where it stops. A lock that quietly guarantees less than a
	// reader assumes is worse than one that names its limit.
	if !strings.Contains(got, "does NOT pin: the bytes") {
		t.Errorf("the lock does not say what it leaves unpinned:\n%s", got)
	}
}

// A pantry that is a plain directory is a legitimate thing to lock against —
// an unpacked tarball, a fixture. But the lock must not claim a revision it
// does not have.
func TestLockSaysWhenThereIsNoRevisionToRecord(t *testing.T) {
	p, _ := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	var out, errb bytes.Buffer
	runLock([]string{"--pantry", p, "lib.org"}, &out, &errb)
	if !strings.Contains(out.String(), "not a git checkout") {
		t.Errorf("a non-git pantry was not reported:\n%s", out.String())
	}
}

// A project whose version cannot be resolved is a HOLE, and a lock with a
// hole must not read as success — the next person would commit it.
func TestAnUnresolvedProjectFailsTheLock(t *testing.T) {
	p, _ := lockbed(t, map[string]string{
		"lib.org": "distributable:\n  url: https://x/v{{version}}.tgz\nbuild:\n  script: [make]\n",
	})
	var out, errb bytes.Buffer
	code := runLock([]string{"--pantry", p, "lib.org"}, &out, &errb)
	if code == 0 {
		t.Error("a lock with an unresolved project reported success")
	}
	if !strings.Contains(errb.String(), "unresolved: lib.org") {
		t.Errorf("the hole was not named: %q", errb.String())
	}
}

func TestLockRefusesAnEmptyRequest(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runLock(nil, &out, &errb); code != 2 {
		t.Fatalf("code=%d; want 2", code)
	}
	if !strings.Contains(errb.String(), "not an empty lock, it is a mistake") {
		t.Errorf("stderr=%q", errb.String())
	}
}

// Go's flag package stops at the first non-flag argument, so a flag after a
// project name is silently a project name. Measured on myself within a
// minute of writing the command.
func TestLockRefusesAFlagAfterAProjectName(t *testing.T) {
	p, _ := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "lib.org", "-o", "/tmp/x"}, &out, &errb); code != 2 {
		t.Fatalf("code=%d; want 2", code)
	}
	if !strings.Contains(errb.String(), "comes after a project name") {
		t.Errorf("stderr=%q", errb.String())
	}
}

func TestLockWritesToAFileWhenAsked(t *testing.T) {
	p, files := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "-o", "/tmp/out.hcl", "lib.org"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	b, ok := (*files)["/tmp/out.hcl"]
	if !ok {
		t.Fatalf("nothing was written; files=%v", *files)
	}
	if !strings.Contains(string(b), `"lib.org" = "1.2.4"`) {
		t.Errorf("file=%q", b)
	}
	if out.Len() != 0 {
		t.Errorf("it wrote to the file AND to stdout: %q", out.String())
	}
	if !strings.Contains(errb.String(), "1 project(s) pinned") {
		t.Errorf("it did not say what it wrote: %q", errb.String())
	}
}

func TestLockReportsAWriteFailure(t *testing.T) {
	p, _ := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	osWriteFile = func(string, []byte, os.FileMode) error { return errors.New("read-only file system") }
	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "-o", "/x", "lib.org"}, &out, &errb); code != 1 {
		t.Fatalf("code=%d; want 1", code)
	}
	if !strings.Contains(errb.String(), "read-only file system") {
		t.Errorf("stderr=%q", errb.String())
	}
}

// A closure that read nothing is a failure, not a clean empty lock.
func TestLockRefusesAnEmptyClosure(t *testing.T) {
	p, _ := lockbed(t, map[string]string{})
	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "nope.org"}, &out, &errb); code != 1 {
		t.Fatalf("code=%d; want 1", code)
	}
	if !strings.Contains(errb.String(), "is a failure and not a clean result") {
		t.Errorf("stderr=%q", errb.String())
	}
}

func TestLockRejectsBadFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runLock([]string{"-nope"}, &out, &errb); code != 2 {
		t.Errorf("code=%d; want 2", code)
	}
}

// An overlay is recorded too, and a real git checkout gives a real hash.
// Both halves matter: the overlay line only appears when one is given, and
// the hash is what makes the lock replayable.
func TestLockRecordsBothRevisions(t *testing.T) {
	p, _ := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	ov := t.TempDir()

	// A real repository, because `(not a git checkout)` is the other branch
	// and asserting the hash means producing one.
	repo, err := gogit.PlainInit(ov, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ov, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("x"); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("c", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@e", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "--overlay", ov, "lib.org"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "# overlay: "+h.String()) {
		t.Errorf("the overlay revision is not recorded:\n%s", out.String())
	}
}

// An empty directory name is not a checkout and must not be reported as a
// broken one: `--overlay ""` means "no overlay", not "an overlay I failed to
// read".
func TestGitRevOfDistinguishesAbsentFromUnreadable(t *testing.T) {
	if got := gitRevOf(""); got != "(none)" {
		t.Errorf("gitRevOf(\"\") = %q; want (none)", got)
	}
	if got := gitRevOf(t.TempDir()); !strings.Contains(got, "not a git checkout") {
		t.Errorf("a plain directory = %q", got)
	}
	// A repository with no commit yet has no HEAD to record.
	empty := t.TempDir()
	if _, err := gogit.PlainInit(empty, false); err != nil {
		t.Fatal(err)
	}
	if got := gitRevOf(empty); got != "(no HEAD)" {
		t.Errorf("an unborn HEAD = %q; want (no HEAD)", got)
	}
}

// A project in the closure whose recipe cannot be read is a hole like any
// other, and must be named rather than dropped.
func TestLockNamesAProjectWhoseRecipeVanishes(t *testing.T) {
	p, _ := lockbed(t, map[string]string{
		"app.org": "dependencies:\n  lib.org: '*'\n" + lockableRecipe,
		"lib.org": lockableRecipe,
	})
	// Remove lib.org AFTER the closure can see it: the walk reads it, the
	// per-project resolve cannot.
	prev := recipeLoader
	recipeLoader = func(set *logical.Set, overlay, pantry, proj string) (*pantry.Recipe, string, error) {
		if proj == "lib.org" {
			return nil, "", errors.New("vanished")
		}
		return prev(set, overlay, pantry, proj)
	}
	t.Cleanup(func() { recipeLoader = prev })

	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "app.org"}, &out, &errb); code != 1 {
		t.Fatalf("code=%d; want 1", code)
	}
	if !strings.Contains(errb.String(), "unresolved: lib.org (no recipe:") {
		t.Errorf("stderr=%q", errb.String())
	}
}

// An --overrides directory that does not parse is a startup error, not a lock
// with a silent hole in its overrides: a run that ignored the overrides would
// pin the versions the UNOVERRIDDEN recipes resolve to, which is a different
// answer wearing the same shape.
func TestLockRefusesUnparseableOverrides(t *testing.T) {
	p, _ := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	ov := t.TempDir()
	if err := os.WriteFile(filepath.Join(ov, "broken.hcl"), []byte("project \"x\" {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runLock([]string{"--pantry", p, "--overrides", ov, "lib.org"}, &out, &errb); code != 2 {
		t.Fatalf("code=%d; want 2 (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "lock:") {
		t.Errorf("the refusal does not name the command: %q", errb.String())
	}
}

// TestLockDispatch covers the main-loop `case "lock"` route.
func TestLockDispatch(t *testing.T) {
	p, _ := lockbed(t, map[string]string{"lib.org": lockableRecipe})
	code, out, errs := run2(t, "lock", "--pantry", p, "lib.org")
	if code != 0 {
		t.Fatalf("dispatch lock code=%d err=%q", code, errs)
	}
	if !strings.Contains(out, `locked = {`) {
		t.Errorf("dispatch lock out=%q", out)
	}
}
