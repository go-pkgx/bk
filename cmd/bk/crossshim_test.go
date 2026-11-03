package main

import (
	"os"
	"path/filepath"
	"testing"
)

// shimDir stages a fake shim directory and a PATH holding dir plus the given
// executables, returning the path the shim would be invoked as.
func shimDir(t *testing.T, shim string, onPath ...string) (self string) {
	t.Helper()
	libexec := t.TempDir()
	bin := t.TempDir()
	self = filepath.Join(libexec, shim)
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range onPath {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", libexec+string(os.PathListSeparator)+bin)
	return self
}

// ⛔⛔ A SHIM DRIVES THE NAME IT WAS INVOKED AS.
//
// bk sets no $BK_CC for a windows cross build — the compiler comes from the
// recipe's llvm-mingw dep — and compilerShimsFor shims the triple-prefixed
// spellings, which go FIRST on PATH. shimBase reduced
// `x86_64-w64-mingw32-gcc` to `gcc`, so the shim shadowed llvm-mingw's real
// compiler and then delegated to a name that bottle does not ship:
//
//	bk gcc: gcc is not on PATH outside …/build.libexec
//
// Measured on go-pkgx/packages' windows-c lane building zlib.net 1.3.2.
func TestAWindowsCrossShimDrivesTheTriplePrefixedCompiler(t *testing.T) {
	t.Setenv("BK_CC", "")
	const shim = "x86_64-w64-mingw32-gcc"
	self := shimDir(t, shim, shim) // llvm-mingw ships exactly this name
	if got := fallbackDriver(shim, self); got != shim {
		t.Errorf("fallbackDriver(%q) = %q, want the name it was invoked as", shim, got)
	}
}

// ⛔ AND STRIPPING THE TRIPLE IS STILL RIGHT WHEN NOTHING SHIPS THAT NAME.
//
// The control, without which the fix could be "always use the exact name",
// which would break the case compilerShimsFor was written for: autoconf probes
// `x86_64-pc-linux-gnu-gcc`, nothing installs it, and reducing to `gcc` is
// what makes libisl's configure find a compiler at all.
func TestATripleNobodyShipsStillFallsBackToTheBareCompiler(t *testing.T) {
	t.Setenv("BK_CC", "")
	const shim = "x86_64-pc-linux-gnu-gcc"
	self := shimDir(t, shim, "gcc") // only the bare compiler exists
	if got := fallbackDriver(shim, self); got != "gcc" {
		t.Errorf("fallbackDriver(%q) = %q, want the bare gcc", shim, got)
	}
}

// An unprefixed shim is unaffected either way: it already named its own
// compiler, and both candidates are the same string.
func TestABareShimIsUnchanged(t *testing.T) {
	t.Setenv("BK_CC", "")
	self := shimDir(t, "cc", "cc")
	if got := fallbackDriver("cc", self); got != "cc" {
		t.Errorf("fallbackDriver(cc) = %q", got)
	}
}

// When NEITHER name is installed the fallback still names the base, so the
// error the caller prints is about a real lookup rather than a half-choice.
func TestNeitherNameInstalledFallsBackToTheBase(t *testing.T) {
	t.Setenv("BK_CC", "")
	const shim = "x86_64-w64-mingw32-gcc"
	self := shimDir(t, shim) // nothing but the shim itself
	if got := fallbackDriver(shim, self); got != "gcc" {
		t.Errorf("fallbackDriver(%q) = %q, want %q", shim, got, "gcc")
	}
}
