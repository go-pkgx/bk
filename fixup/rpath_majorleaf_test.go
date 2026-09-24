package fixup

import (
	"os"
	"path/filepath"
	"testing"
)

// mkLib lays out one library directory: a real file plus the symlinks named.
func mkLib(t *testing.T, real string, links ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, real), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if err := os.Symlink(real, filepath.Join(dir, l)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The zlib layout that broke cairo: the major link is preferred over both the
// full-version file and the unversioned link.
func TestMajorLeafPrefersTheMajorSoname(t *testing.T) {
	dir := mkLib(t, "libz.1.3.2.dylib", "libz.1.dylib", "libz.dylib")
	if got := majorLeaf(filepath.Join(dir, "libz.1.3.2.dylib")); got != "libz.1.dylib" {
		t.Fatalf("majorLeaf = %q, want libz.1.dylib", got)
	}
}

// A library that ships no soname link keeps the name the build chose: there is
// nothing to bind to that would survive the upgrade, and inventing one would
// name a file that is not there.
func TestMajorLeafKeepsTheNameWhenNoLinkShips(t *testing.T) {
	dir := mkLib(t, "libfoo.1.2.3.dylib")
	if got := majorLeaf(filepath.Join(dir, "libfoo.1.2.3.dylib")); got != "libfoo.1.2.3.dylib" {
		t.Fatalf("majorLeaf = %q, want the original name", got)
	}
}

// The unversioned link alone is NOT taken: it binds across majors, a wider
// promise than the majored directory makes.
func TestMajorLeafRefusesTheUnversionedLink(t *testing.T) {
	dir := mkLib(t, "libfoo.1.2.3.dylib", "libfoo.dylib")
	if got := majorLeaf(filepath.Join(dir, "libfoo.1.2.3.dylib")); got != "libfoo.1.2.3.dylib" {
		t.Fatalf("majorLeaf = %q, want the original name", got)
	}
}

// A link with the right SHAPE that points at a different file is not used —
// the check is what it resolves to, not what it is called.
func TestMajorLeafRefusesALinkToAnotherFile(t *testing.T) {
	dir := mkLib(t, "libfoo.1.2.3.dylib")
	if err := os.WriteFile(filepath.Join(dir, "libfoo.9.9.9.dylib"), []byte("y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libfoo.9.9.9.dylib", filepath.Join(dir, "libfoo.1.dylib")); err != nil {
		t.Fatal(err)
	}
	if got := majorLeaf(filepath.Join(dir, "libfoo.1.2.3.dylib")); got != "libfoo.1.2.3.dylib" {
		t.Fatalf("majorLeaf = %q, want the original name", got)
	}
}

// A missing file yields its own name rather than an error: the reference may
// already be stale, and this must not be the thing that fails the build. Both
// spellings of absent — no directory, and a directory without the file — reach
// it, which is why the directory is read before the file is resolved.
func TestMajorLeafOnAMissingFile(t *testing.T) {
	if got := majorLeaf(filepath.Join(t.TempDir(), "libgone.1.2.3.dylib")); got != "libgone.1.2.3.dylib" {
		t.Fatalf("majorLeaf on an empty directory = %q, want the original name", got)
	}
	gone := filepath.Join(t.TempDir(), "nodir", "libgone.1.2.3.dylib")
	if got := majorLeaf(gone); got != "libgone.1.2.3.dylib" {
		t.Fatalf("majorLeaf with no directory = %q, want the original name", got)
	}
}

func TestIsMajorSoname(t *testing.T) {
	for _, tc := range []struct {
		leaf, cand string
		want       bool
	}{
		{"libz.1.3.1.dylib", "libz.1.dylib", true},
		{"libz.1.3.1.dylib", "libz.1.3.dylib", true},
		{"libz.1.3.1.dylib", "libz.dylib", false},
		{"libz.1.3.1.dylib", "libz.1.3.1.dylib", false}, // itself is no shorter
		{"libz.1.3.1.dylib", "libpng.1.dylib", false},
		{"libpcre2-8.0.dylib", "libpcre2-8.dylib", false}, // no numeric component left
		{"libz.1.3.1.dylib", "libz.1.so", false},
		{"libz.1.3.1.so", "libz.1.dylib", false},
		// An empty component is not a major.
		{"libz..1.dylib", "libz..dylib", false},
		// Nor is a component that is not a number.
		{"libfoo.1a.2.dylib", "libfoo.1a.dylib", false},
	} {
		if got := isMajorSoname(tc.leaf, tc.cand); got != tc.want {
			t.Errorf("isMajorSoname(%q, %q) = %v, want %v", tc.leaf, tc.cand, got, tc.want)
		}
	}
}
