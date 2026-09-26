package fixup

import (
	"os"
	"sync"
	"testing"
)

// TestSystemHeaderNamesWhenADirectoryCannotBeRead covers the `continue` that
// keeps an unreadable system include directory from emptying the union.
//
// Whether it runs at all used to depend on the HOST: systemIncludeDirs always
// names /usr/include, which a macOS without the Command Line Tools does not
// have (error -> continue) and a linux container does (no error -> the line is
// never reached). Coverage is judged on linux, so the line read as dead there,
// and the gate stayed quiet because the TOTAL still rounded to 100.0%.
//
// The floor matters as much as the line: where the filesystem cannot be read,
// the curated list is the whole answer, and a `continue` that silently became
// a `return` would let a flatten shadow a system header on exactly the hosts
// with the least to check against -- a from-scratch rootfs, or a cross build.
func TestSystemHeaderNamesWhenADirectoryCannotBeRead(t *testing.T) {
	defer restore()
	systemHeadersOnce = sync.Once{}
	systemHeadersCache = nil
	t.Cleanup(func() { systemHeadersOnce = sync.Once{}; systemHeadersCache = nil })
	osReadDir = func(string) ([]os.DirEntry, error) { return nil, errInject }

	got := systemHeaderNames()
	if len(got) != len(systemHeaders) {
		t.Errorf("got %d names, want the curated floor of %d", len(got), len(systemHeaders))
	}
	for k := range systemHeaders {
		if !got[k] {
			t.Errorf("the floor lost %q when the filesystem could not be read", k)
		}
	}
}
