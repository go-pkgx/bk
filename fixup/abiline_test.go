package fixup

import (
	"os"
	"path/filepath"
	"testing"
)

// mkABIStore lays out <pkgxDir>/<project>/v<ver>/lib plus the abi- links named.
func mkABIStore(t *testing.T, project, ver string, sonames ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(project), "v"+ver, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range sonames {
		if err := os.Symlink("v"+ver, filepath.Join(dir, filepath.FromSlash(project), "abi-"+s)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAbiLine(t *testing.T) {
	dir := mkABIStore(t, "gnome.org/libxml2", "2.15.4", "libxml2.16.dylib")
	orig := filepath.Join(dir, "gnome.org/libxml2/v2.15.4/lib/libxml2.16.dylib")

	got, ok := abiLine(orig, "libxml2.16.dylib", dir)
	if !ok {
		t.Fatal("a store offering the link did not answer")
	}
	want := filepath.Join(dir, "gnome.org/libxml2/abi-libxml2.16.dylib/lib/libxml2.16.dylib")
	if got != want {
		t.Errorf("abiLine = %q, want %q", got, want)
	}
}

// Only when the link is really there. A dependency published before bottle
// wrote them keeps the v<major> form and behaves exactly as before, which is
// what lets this land while most of the catalogue predates it.
func TestAbiLineWithoutTheLink(t *testing.T) {
	dir := mkABIStore(t, "gnome.org/libxml2", "2.15.4")
	orig := filepath.Join(dir, "gnome.org/libxml2/v2.15.4/lib/libxml2.16.dylib")
	if _, ok := abiLine(orig, "libxml2.16.dylib", dir); ok {
		t.Error("a missing link was used anyway")
	}
}

// A dangling link is not a line anybody can bind to.
func TestAbiLineWithADanglingLink(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "acme.org", "thing")
	if err := os.MkdirAll(filepath.Join(proj, "v1.0.0", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v9.9.9", filepath.Join(proj, "abi-libthing.1.dylib")); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(proj, "v1.0.0", "lib", "libthing.1.dylib")
	if _, ok := abiLine(orig, "libthing.1.dylib", dir); ok {
		t.Error("a dangling link was used")
	}
}

func TestAbiLineRefusals(t *testing.T) {
	dir := mkABIStore(t, "acme.org/thing", "1.0.0", "libthing.1.dylib")
	orig := filepath.Join(dir, "acme.org/thing/v1.0.0/lib/libthing.1.dylib")
	for _, tc := range []struct {
		name, orig, leaf, pkgx string
	}{
		{"no pkgxDir", orig, "libthing.1.dylib", ""},
		{"no leaf", orig, "", dir},
		{"outside the store", "/elsewhere/acme.org/thing/v1.0.0/lib/libthing.1.dylib", "libthing.1.dylib", dir},
		{"no version directory", filepath.Join(dir, "acme.org/thing/lib/libthing.1.dylib"), "libthing.1.dylib", dir},
	} {
		if _, ok := abiLine(tc.orig, tc.leaf, tc.pkgx); ok {
			t.Errorf("%s: answered anyway", tc.name)
		}
	}
}

// The rewrite, end to end, without needing a linker — so the linux lane
// exercises it too. With an abi- link in the store the reference binds to the
// ABI line; without one it keeps the v<major> form it has always had.
func TestRewriteBindsToTheABILineWhenOffered(t *testing.T) {
	for _, withLink := range []bool{true, false} {
		name := "with the link"
		if !withLink {
			name = "without it"
		}
		t.Run(name, func(t *testing.T) {
			pkgx := filepath.Join(t.TempDir(), ".pkgx")
			dep := filepath.Join(pkgx, "abi.example", "v1.0.0", "lib")
			if err := os.MkdirAll(dep, 0o755); err != nil {
				t.Fatal(err)
			}
			if withLink {
				if err := os.Symlink("v1.0.0", filepath.Join(pkgx, "abi.example", "abi-libfoo.1.dylib")); err != nil {
					t.Fatal(err)
				}
			}
			prefix := filepath.Join(pkgx, "other.org", "bar", "v2.0.0")
			exe := filepath.Join(prefix, "bin", "bar")
			place(t, exe,
				machoCmd{lcRpath, "@loader_path/../../../.."},
				machoCmd{lcLoadDylib, filepath.Join(dep, "libfoo.1.dylib")},
			)
			if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: pkgx}); err != nil {
				t.Fatal(err)
			}
			got, err := ReadMachoStrings(exe)
			if err != nil {
				t.Fatal(err)
			}
			want := "@rpath/abi.example/abi-libfoo.1.dylib/lib/libfoo.1.dylib"
			if !withLink {
				want = "@rpath/abi.example/v1/lib/libfoo.1.dylib"
			}
			found := false
			for _, s := range got {
				if s == want {
					found = true
				}
			}
			if !found {
				t.Errorf("reference is not %q: %q", want, got)
			}
		})
	}
}
