package fixup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixStagingScripts uses the real shapes found in the published registry:
// openssl's c_rehash and perl's perlivp both bake the +brewing staging path,
// which exists on no machine at all once the tree is staged.
func TestFixStagingScripts(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "openssl.org", "v3.0.19")
	staging := prefix + "+brewing"
	mustMkdir(t, filepath.Join(prefix, "bin"))
	mustMkdir(t, filepath.Join(prefix, "libexec", "nested"))

	rehash := filepath.Join(prefix, "bin", "c_rehash")
	mustWrite(t, rehash, "#!/usr/bin/env perl\nmy $dir = \""+staging+"/ssl\";\nmy $prefix = \""+staging+"\";\n", 0o755)
	nested := filepath.Join(prefix, "libexec", "nested", "helper")
	mustWrite(t, nested, "#!/bin/sh\nexec \""+staging+"/bin/real\" \"$@\"\n", 0o755)
	// An ELF-shaped file carrying the same bytes must NOT be rewritten: the
	// replacement is shorter and every offset after it would shift.
	binary := filepath.Join(prefix, "bin", "openssl")
	elf := "\x7fELF\x02\x01\x01" + staging + "/lib\x00"
	mustWrite(t, binary, elf, 0o755)
	// A script that never mentions the staging prefix is left byte-identical.
	clean := filepath.Join(prefix, "bin", "plain")
	mustWrite(t, clean, "#!/bin/sh\necho hello\n", 0o755)

	if err := fixStagingScripts(prefix, staging, func(string, ...any) {}); err != nil {
		t.Fatalf("fixStagingScripts: %v", err)
	}

	got := read(t, rehash)
	if strings.Contains(got, "+brewing") {
		t.Errorf("the staging prefix survived in c_rehash:\n%s", got)
	}
	if !strings.Contains(got, prefix+"/ssl") {
		t.Errorf("c_rehash does not point at the final prefix:\n%s", got)
	}
	if g := read(t, nested); strings.Contains(g, "+brewing") {
		t.Errorf("a nested script was not visited:\n%s", g)
	}
	if g := read(t, binary); g != elf {
		t.Error("the ELF was rewritten; that shifts every offset after the string")
	}
	if g := read(t, clean); g != "#!/bin/sh\necho hello\n" {
		t.Errorf("an unaffected script was touched: %q", g)
	}
	// The mode survives the rewrite — a script that loses +x is worse than one
	// with a wrong path in it.
	if fi, err := os.Stat(rehash); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v err %v, want 0755", fi.Mode().Perm(), err)
	}
}

func TestFixStagingScriptsNoStaging(t *testing.T) {
	// No staging prefix, or one equal to the final prefix: nothing to do, and
	// in particular no walk that could rewrite anything.
	if err := fixStagingScripts("/nope", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := fixStagingScripts("/nope", "/nope", nil); err != nil {
		t.Fatal(err)
	}
}

func TestFixStagingScriptsErrors(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "p")
	mustMkdir(t, filepath.Join(prefix, "bin"))
	mustWrite(t, filepath.Join(prefix, "bin", "s"), "#!/bin/sh\n"+prefix+"+brewing/x\n", 0o755)

	boom := errors.New("boom")
	t.Run("readdir", func(t *testing.T) {
		defer restoreReadDir(osReadDir)
		osReadDir = func(string) ([]os.DirEntry, error) { return nil, boom }
		if err := fixStagingScripts(prefix, prefix+"+brewing", nil); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("readfile", func(t *testing.T) {
		defer restoreReadFile(osReadFile)
		osReadFile = func(string) ([]byte, error) { return nil, boom }
		if err := fixStagingScripts(prefix, prefix+"+brewing", nil); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		defer restoreWriteFile(osWriteFile)
		osWriteFile = func(string, []byte, os.FileMode) error { return boom }
		if err := fixStagingScripts(prefix, prefix+"+brewing", func(string, ...any) {}); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("nested dir", func(t *testing.T) {
		mustMkdir(t, filepath.Join(prefix, "bin", "sub"))
		mustWrite(t, filepath.Join(prefix, "bin", "sub", "s"), "#!/bin/sh\n"+prefix+"+brewing/x\n", 0o755)
		calls := 0
		defer restoreReadDir(osReadDir)
		real := osReadDir
		osReadDir = func(p string) ([]os.DirEntry, error) {
			calls++
			if calls == 2 {
				return nil, boom
			}
			return real(p)
		}
		if err := fixStagingScripts(prefix, prefix+"+brewing", func(string, ...any) {}); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
}

// TestRewriteFileEmptyPrefix: an empty prefix must not splice the replacement
// between every character, which ReplaceAll on "" does.
func TestRewriteFileEmptyPrefix(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "abc/staging/def", 0o644)
	if err := rewriteFile(p, "/staging", "", "/final", func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "abc/final/def" {
		t.Fatalf("got %q", got)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, s string, m os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), m); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func restoreReadDir(f func(string) ([]os.DirEntry, error)) { osReadDir = f }
func restoreReadFile(f func(string) ([]byte, error))       { osReadFile = f }
func restoreWriteFile(f func(string, []byte, os.FileMode) error) {
	osWriteFile = f
}

// TestFixUpPropagatesStagingScriptError: FixUp must stop on the staging-script
// pass like it does on every other, rather than bottling a tree it failed to
// fix.
func TestFixUpPropagatesStagingScriptError(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "p", "v1")
	mustMkdir(t, filepath.Join(prefix, "bin"))
	mustWrite(t, filepath.Join(prefix, "bin", "s"), "#!/bin/sh\n"+prefix+"+brewing/x\n", 0o755)

	boom := errors.New("boom")
	defer restoreWriteFile(osWriteFile)
	osWriteFile = func(string, []byte, os.FileMode) error { return boom }

	err := FixUp(Options{
		Prefix:       prefix,
		BuildInstall: prefix + "+brewing",
		Platform:     "linux",
		Skips:        []string{"fix-patchelf"},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the staging-script write error", err)
	}
}
