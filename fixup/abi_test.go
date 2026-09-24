package fixup

import (
	"debug/elf"
	"os"
	"testing"
)

// An ELF states its ABI name in DT_SONAME, and the basename is what ld.so
// matches on.
func TestSonameOfELF(t *testing.T) {
	p := buildELFTag(t, elf.DT_SONAME, "/build/dir/libfoo.so.1", "libc.so.6", 32)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := SonameOf(raw); got != "libfoo.so.1" {
		t.Errorf("SonameOf = %q, want libfoo.so.1", got)
	}
}

// An ELF with no DT_SONAME is not a shared library anyone binds to by name.
func TestSonameOfELFWithout(t *testing.T) {
	p := buildELF64LE(t, "$ORIGIN/../lib", "libc.so.6", 32)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := SonameOf(raw); got != "" {
		t.Errorf("SonameOf = %q, want empty", got)
	}
}

// Anything that is not an object file at all — a script, an archive, a
// truncated read — yields nothing rather than an error: a bottle is full of
// them and none is a shared library.
func TestSonameOfNonObject(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		[]byte("#!/bin/sh\necho hi\n"),
		[]byte("!<arch>\n"),
		[]byte("\x7fELF truncated"),
		[]byte{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O magic and nothing after it
	} {
		if got := SonameOf(b); got != "" {
			t.Errorf("SonameOf(%q) = %q, want empty", b, got)
		}
	}
}

func TestSortedUnique(t *testing.T) {
	got := SortedUnique([]string{"libz.1.dylib", "", "libа.dylib", "libz.1.dylib", "liba.dylib"})
	want := []string{"liba.dylib", "libz.1.dylib", "libа.dylib"}
	if len(got) != len(want) {
		t.Fatalf("SortedUnique = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SortedUnique = %v, want %v", got, want)
		}
	}
}
