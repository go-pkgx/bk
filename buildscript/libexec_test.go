package buildscript

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWriteLibexecHappy proves WriteLibexec creates the dir and drops every
// embedded shim there, executable, with the exact embedded contents.
func TestWriteLibexecHappy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "libexec")
	if err := WriteLibexec(dir); err != nil {
		t.Fatalf("WriteLibexec: %v", err)
	}
	shim := filepath.Join(dir, "fix-shebangs.ts")
	fi, err := os.Stat(shim)
	if err != nil {
		t.Fatalf("stat shim: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("shim not executable: %v", fi.Mode())
	}
	got, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(fixShebangs) {
		t.Error("written shim != embedded contents")
	}
	// the embed must actually carry the shim, not an empty file
	if len(fixShebangs) == 0 {
		t.Error("embedded fix-shebangs.ts is empty")
	}
}

func TestWriteLibexecErrors(t *testing.T) {
	restore := func() { mkdirAll, writeFile = os.MkdirAll, os.WriteFile }

	t.Run("mkdir", func(t *testing.T) {
		defer restore()
		mkdirAll = func(string, os.FileMode) error { return errBoomLibexec }
		if err := WriteLibexec("x"); !errors.Is(err, errBoomLibexec) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		defer restore()
		writeFile = func(string, []byte, os.FileMode) error { return errBoomLibexec }
		if err := WriteLibexec(t.TempDir()); !errors.Is(err, errBoomLibexec) {
			t.Errorf("err = %v", err)
		}
	})
}

var errBoomLibexec = errors.New("boom")

// TestFixShebangsShim runs the materialised shim through /bin/sh over a battery
// of sample files and asserts the rewrite matches upstream fix-shebangs.ts:
// an absolute interpreter is rewritten to `#!/usr/bin/env <basename>` (args
// dropped, body preserved), while env/sh shebangs, binaries, no-shebang files
// and missing paths are left untouched.
func TestFixShebangsShim(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh available: %v", err)
	}
	dir := t.TempDir()
	if err := WriteLibexec(dir); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "fix-shebangs.ts")

	work := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(work, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// a script with an absolute interpreter + interpreter args → rewritten
	script := write("script.py", "#!/opt/pkgx/python.org/v3.11/bin/python3 -u\nprint('hi')\n", 0o755)
	// an already-relocatable env shebang → untouched
	envScript := write("env.py", "#!/usr/bin/env python3\nprint('hi')\n", 0o755)
	// a /bin/sh shebang → untouched
	shScript := write("plain.sh", "#!/bin/sh\necho hi\n", 0o755)
	// a binary (ELF magic) → skipped
	binary := write("data.bin", "\x7fELF\x02\x01\x01\x00rest", 0o755)
	// a file with no shebang → untouched
	noShebang := write("notes.txt", "just some text\nno shebang\n", 0o644)
	// a shebang-only file with no trailing newline → rewritten, still valid
	oneLine := write("oneliner", "#!/some/abs/bin/perl", 0o755)
	// a missing path argument → silently skipped, no error
	missing := filepath.Join(work, "does-not-exist")

	cmd := exec.Command(sh, shim, script, envScript, shScript, binary, noShebang, oneLine, missing)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shim failed: %v\n%s", err, out)
	}

	assert := func(path, want string) {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s =\n%q\nwant\n%q", filepath.Base(path), got, want)
		}
	}
	assert(script, "#!/usr/bin/env python3\nprint('hi')\n")
	assert(envScript, "#!/usr/bin/env python3\nprint('hi')\n")
	assert(shScript, "#!/bin/sh\necho hi\n")
	assert(binary, "\x7fELF\x02\x01\x01\x00rest")
	assert(noShebang, "just some text\nno shebang\n")
	assert(oneLine, "#!/usr/bin/env perl\n")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("missing path should still not exist: %v", err)
	}

	// the rewritten script keeps its executable bit
	if fi, _ := os.Stat(script); fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("rewritten script lost +x: %v", fi.Mode())
	}
}

// TestFixShebangsShimMalformed proves a shebang whose interpreter is not an
// absolute path is a hard error (matching upstream's thrown exception).
func TestFixShebangsShimMalformed(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh available: %v", err)
	}
	dir := t.TempDir()
	if err := WriteLibexec(dir); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("#!python3\nx\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, filepath.Join(dir, "fix-shebangs.ts"), bad)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("expected failure on malformed shebang, got:\n%s", out)
	}
}
