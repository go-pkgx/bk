package overrides

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const recipe = "dependencies:\n  openssl.org: ^1.1\n  zlib.net: ^1.2\n"

// modPatch changes the openssl constraint — the shape of a real override.
const modPatch = `diff --git a/projects/foo.org/package.yml b/projects/foo.org/package.yml
--- a/projects/foo.org/package.yml
+++ b/projects/foo.org/package.yml
@@ -1,3 +1,3 @@
 dependencies:
-  openssl.org: ^1.1
+  openssl.org: ^3
   zlib.net: ^1.2
`

// writeFile drops content at dir/rel, creating parents.
func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// initPantry makes a git checkout holding one committed recipe (plus any extra
// committed files), like the pantry clone the factory patches.
func initPantry(t *testing.T, extra ...string) string {
	t.Helper()
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	files := append([]string{"projects/foo.org/package.yml", recipe}, extra...)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(files); i += 2 {
		writeFile(t, root, files[i], files[i+1])
		if _, err := wt.Add(files[i]); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "t", Email: "t@example.invalid", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	return root
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestApplyModifyIsIdempotent proves the headline behaviour: a patch applies to
// the checkout, and applying the SAME patch again to the SAME (already-patched)
// checkout still yields exactly one applied patch and identical content — the
// reset step makes re-runs against a persistent clone safe.
func TestApplyModifyIsIdempotent(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	writeFile(t, dir, "foo.org-openssl3.patch", modPatch)

	want := "dependencies:\n  openssl.org: ^3\n  zlib.net: ^1.2\n"
	for pass := 1; pass <= 2; pass++ {
		var logs, warns []string
		res, err := Apply(Options{
			Dir:  dir,
			Root: root,
			Log:  func(s string) { logs = append(logs, s) },
			Warn: func(s string) { warns = append(warns, s) },
		})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(res.Applied) != 1 || res.Applied[0] != "foo.org-openssl3.patch" {
			t.Fatalf("pass %d: applied = %v", pass, res.Applied)
		}
		if len(res.Skipped) != 0 {
			t.Fatalf("pass %d: skipped = %v", pass, res.Skipped)
		}
		if len(warns) != 0 {
			t.Fatalf("pass %d: warns = %v", pass, warns)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "override applied: foo.org-openssl3.patch") {
			t.Fatalf("pass %d: logs = %v", pass, logs)
		}
		if got := read(t, filepath.Join(root, "projects/foo.org/package.yml")); got != want {
			t.Fatalf("pass %d: content = %q, want %q", pass, got, want)
		}
	}
}

// TestApplyOrderAndNilCallbacks: patches apply in sorted order, and nil
// Log/Warn callbacks are simply dropped.
func TestApplyOrderAndNilCallbacks(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	// b applies on top of a's result, so a wrong order would fail to apply. It
	// is also a bare unified diff (no `diff --git` header) — the a//b prefix
	// still has to resolve against the pantry root.
	writeFile(t, dir, "a.patch", modPatch)
	writeFile(t, dir, "b.patch", `--- a/projects/foo.org/package.yml
+++ b/projects/foo.org/package.yml
@@ -1,3 +1,3 @@
 dependencies:
   openssl.org: ^3
-  zlib.net: ^1.2
+  zlib.net: ^1.3
`)
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Applied, ",") != "a.patch,b.patch" {
		t.Fatalf("applied = %v", res.Applied)
	}
	want := "dependencies:\n  openssl.org: ^3\n  zlib.net: ^1.3\n"
	if got := read(t, filepath.Join(root, "projects/foo.org/package.yml")); got != want {
		t.Fatalf("content = %q", got)
	}
}

// TestApplyNewAndDelete covers the add-file and delete-file forms, including
// the executable mode carried by a new-file diff.
func TestApplyNewAndDelete(t *testing.T) {
	root := initPantry(t, "projects/foo.org/old.sh", "gone\n")
	dir := t.TempDir()
	writeFile(t, dir, "new.patch", `diff --git a/projects/foo.org/fix.sh b/projects/foo.org/fix.sh
new file mode 100755
--- /dev/null
+++ b/projects/foo.org/fix.sh
@@ -0,0 +1,2 @@
+#!/bin/sh
+echo hi
`)
	writeFile(t, dir, "rm.patch", `diff --git a/projects/foo.org/old.sh b/projects/foo.org/old.sh
deleted file mode 100644
--- a/projects/foo.org/old.sh
+++ /dev/null
@@ -1 +0,0 @@
-gone
`)
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("res = %+v", res)
	}
	added := filepath.Join(root, "projects/foo.org/fix.sh")
	if got := read(t, added); got != "#!/bin/sh\necho hi\n" {
		t.Fatalf("added content = %q", got)
	}
	fi, err := os.Stat(added)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("added mode = %v, want 0755", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "projects/foo.org/old.sh")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still present: %v", err)
	}
}

// TestApplySkipsBadPatches: unreadable, unparsable, empty and non-applying
// patches are each skipped loudly, and the good one still applies.
func TestApplySkipsBadPatches(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "unreadable.patch"), 0o755); err != nil { // a dir reads as EISDIR
		t.Fatal(err)
	}
	writeFile(t, dir, "unparsable.patch", "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,4 +1,4 @@\n truncated\n")
	writeFile(t, dir, "empty.patch", "just a note, no diff at all\n")
	writeFile(t, dir, "stale.patch", `--- a/projects/foo.org/package.yml
+++ b/projects/foo.org/package.yml
@@ -1,3 +1,3 @@
 dependencies:
-  openssl.org: ^0.9
+  openssl.org: ^3
   zlib.net: ^1.2
`)
	writeFile(t, dir, "zgood.patch", modPatch)

	var warns []string
	res, err := Apply(Options{Dir: dir, Root: root, Warn: func(s string) { warns = append(warns, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Applied, ",") != "zgood.patch" {
		t.Fatalf("applied = %v", res.Applied)
	}
	if strings.Join(res.Skipped, ",") != "empty.patch,unparsable.patch,unreadable.patch,stale.patch" {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	for _, want := range []string{"unreadable", "unparsable", "no file diffs", "does not apply"} {
		if !strings.Contains(strings.Join(warns, "\n"), want) {
			t.Fatalf("warns %v missing %q", warns, want)
		}
	}
}

// TestApplyMissingTargetFile: a patch against a file the pantry does not have
// is skipped (the read of the old content fails).
func TestApplyMissingTargetFile(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	writeFile(t, dir, "gone.patch", `--- a/projects/nope.org/package.yml
+++ b/projects/nope.org/package.yml
@@ -1 +1 @@
-a
+b
`)
	var warns []string
	res, err := Apply(Options{Dir: dir, Root: root, Warn: func(s string) { warns = append(warns, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Skipped, ",") != "gone.patch" || len(res.Applied) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(warns[0], "no such file") {
		t.Fatalf("warn = %q", warns[0])
	}
}

func TestApplyNoPatches(t *testing.T) {
	res, err := Apply(Options{Dir: t.TempDir(), Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestApplyBadGlob(t *testing.T) {
	if _, err := Apply(Options{Dir: "bad[", Root: t.TempDir()}); !errors.Is(err, filepath.ErrBadPattern) {
		t.Fatalf("err = %v, want ErrBadPattern", err)
	}
}

// TestApplyResetFailuresAreNonFatal: each way the reset can fail (not a repo /
// bare repo / no commits) warns but still lets the patch apply.
func TestApplyResetFailuresAreNonFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(t *testing.T) string
		want string
	}{
		{"not a repo", func(t *testing.T) string { return t.TempDir() }, "repository does not exist"},
		{"bare repo", func(t *testing.T) string {
			root := t.TempDir()
			if _, err := git.PlainInit(root, true); err != nil {
				t.Fatal(err)
			}
			return root
		}, "worktree not available in a bare repository"},
		{"unborn HEAD", func(t *testing.T) string {
			root := t.TempDir()
			if _, err := git.PlainInit(root, false); err != nil {
				t.Fatal(err)
			}
			return root
		}, "reference not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.root(t)
			writeFile(t, root, "projects/foo.org/package.yml", recipe)
			dir := t.TempDir()
			writeFile(t, dir, "p.patch", modPatch)

			var warns []string
			res, err := Apply(Options{Dir: dir, Root: root, Warn: func(s string) { warns = append(warns, s) }})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(res.Applied, ",") != "p.patch" {
				t.Fatalf("applied = %v (warns %v)", res.Applied, warns)
			}
			if len(warns) != 1 || !strings.Contains(warns[0], "cannot reset pantry files") || !strings.Contains(warns[0], tc.want) {
				t.Fatalf("warns = %v, want %q", warns, tc.want)
			}
		})
	}
}

// TestApplyFilesWriteErrors covers the disk-write failure branches: mkdir,
// write, remove, and the stat that supplies the preserved file mode.
func TestApplyFilesWriteErrors(t *testing.T) {
	boom := errors.New("boom")

	t.Run("mkdir", func(t *testing.T) {
		defer func(f func(string, fs.FileMode) error) { osMkdirAll = f }(osMkdirAll)
		osMkdirAll = func(string, fs.FileMode) error { return boom }
		root := initPantry(t)
		dir := t.TempDir()
		writeFile(t, dir, "p.patch", modPatch)
		res, err := Apply(Options{Dir: dir, Root: root})
		if err != nil || strings.Join(res.Skipped, ",") != "p.patch" {
			t.Fatalf("res = %+v err = %v", res, err)
		}
	})

	t.Run("write", func(t *testing.T) {
		defer func(f func(string, []byte, fs.FileMode) error) { osWriteFile = f }(osWriteFile)
		osWriteFile = func(string, []byte, fs.FileMode) error { return boom }
		root := initPantry(t)
		dir := t.TempDir()
		writeFile(t, dir, "p.patch", modPatch)
		res, err := Apply(Options{Dir: dir, Root: root})
		if err != nil || strings.Join(res.Skipped, ",") != "p.patch" {
			t.Fatalf("res = %+v err = %v", res, err)
		}
	})

	t.Run("remove", func(t *testing.T) {
		root := initPantry(t)
		dir := t.TempDir()
		// deletes a file that is not there → os.Remove fails
		writeFile(t, dir, "rm.patch", `--- a/projects/foo.org/absent.sh
+++ /dev/null
@@ -1 +0,0 @@
-gone
`)
		var warns []string
		res, err := Apply(Options{Dir: dir, Root: root, Warn: func(s string) { warns = append(warns, s) }})
		if err != nil || strings.Join(res.Skipped, ",") != "rm.patch" {
			t.Fatalf("res = %+v err = %v", res, err)
		}
		if !strings.Contains(warns[0], "no such file") {
			t.Fatalf("warn = %q", warns[0])
		}
	})

	t.Run("chmod", func(t *testing.T) {
		defer func(f func(string, fs.FileMode) error) { osChmod = f }(osChmod)
		osChmod = func(string, fs.FileMode) error { return boom }
		root := initPantry(t)
		dir := t.TempDir()
		writeFile(t, dir, "mode.patch", `diff --git a/projects/foo.org/run.sh b/projects/foo.org/run.sh
new file mode 100755
--- /dev/null
+++ b/projects/foo.org/run.sh
@@ -0,0 +1 @@
+hi
`)
		res, err := Apply(Options{Dir: dir, Root: root})
		if err != nil || strings.Join(res.Skipped, ",") != "mode.patch" {
			t.Fatalf("res = %+v err = %v", res, err)
		}
	})
}

// TestApplyChangesModeOfExistingFile: a mode-only-style change reaches an
// already-present file (os.WriteFile alone would keep the old permissions).
func TestApplyChangesModeOfExistingFile(t *testing.T) {
	root := initPantry(t, "projects/foo.org/run.sh", "hi\n")
	dir := t.TempDir()
	writeFile(t, dir, "mode.patch", `diff --git a/projects/foo.org/run.sh b/projects/foo.org/run.sh
old mode 100644
new mode 100755
--- a/projects/foo.org/run.sh
+++ b/projects/foo.org/run.sh
@@ -1 +1 @@
-hi
+hello
`)
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil || strings.Join(res.Applied, ",") != "mode.patch" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	fi, err := os.Stat(filepath.Join(root, "projects/foo.org/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
	}
}

// TestApplyFilesNoPath: a file diff naming neither side is rejected rather than
// written to the checkout root. gitdiff never emits one, so build it directly.
func TestApplyFilesNoPath(t *testing.T) {
	err := applyFiles(t.TempDir(), []*gitdiff.File{{}})
	if err == nil || !strings.Contains(err.Error(), "names no path") {
		t.Fatalf("err = %v", err)
	}
}
