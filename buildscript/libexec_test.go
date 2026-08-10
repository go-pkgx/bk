package buildscript

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteLibexecHappy proves WriteLibexec creates the dir and materialises
// every shim as a symlink pointing at the running executable (bk itself), so
// exec'ing the shim re-enters bk's multi-call dispatch — not a shell script.
func TestWriteLibexecHappy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "libexec")
	if err := WriteLibexec(dir); err != nil {
		t.Fatalf("WriteLibexec: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	shim := filepath.Join(dir, "fix-shebangs.ts")

	// it is a symlink (not a regular file / embedded script)…
	fi, err := os.Lstat(shim)
	if err != nil {
		t.Fatalf("lstat shim: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("shim is not a symlink: %v", fi.Mode())
	}
	// …and it points at the running bk binary.
	target, err := os.Readlink(shim)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != self {
		t.Errorf("shim → %q, want %q", target, self)
	}
	// every declared shim name must be materialised.
	for name := range shims {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("shim %q missing: %v", name, err)
		}
	}
	// every brewkit helper bk satisfies must be present: fix-shebangs.ts
	// (relocatable shebangs), bkpyvenv (new python-venv stage/seal) and the old
	// python-venv.{sh,py}/stubber family. A regression dropping any would break a
	// whole class of recipes.
	for _, want := range []string{"fix-shebangs.ts", "bkpyvenv", "python-venv.sh", "python-venv.py", "python-venv-stubber.sh"} {
		if _, ok := shims[want]; !ok {
			t.Errorf("shim set missing %q", want)
		}
	}
}

func TestWriteLibexecErrors(t *testing.T) {
	restore := func() {
		mkdirAll, osExecutable, osSymlink = os.MkdirAll, os.Executable, os.Symlink
	}

	t.Run("mkdir", func(t *testing.T) {
		defer restore()
		mkdirAll = func(string, os.FileMode) error { return errBoomLibexec }
		if err := WriteLibexec("x"); !errors.Is(err, errBoomLibexec) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("executable", func(t *testing.T) {
		defer restore()
		osExecutable = func() (string, error) { return "", errBoomLibexec }
		if err := WriteLibexec(t.TempDir()); !errors.Is(err, errBoomLibexec) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		defer restore()
		osSymlink = func(string, string) error { return errBoomLibexec }
		if err := WriteLibexec(t.TempDir()); !errors.Is(err, errBoomLibexec) {
			t.Errorf("err = %v", err)
		}
	})
}

var errBoomLibexec = errors.New("boom")
