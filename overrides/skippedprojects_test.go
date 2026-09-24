package overrides

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A patch that does not apply names the project whose recipe it was meant to
// change, so the caller can refuse to build it. A warning in a log cannot be
// acted on; this can.
func TestSkippedProjectsNamesTheRecipe(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	writeFile(t, dir, "stale.patch", `--- a/projects/foo.org/package.yml
+++ b/projects/foo.org/package.yml
@@ -1,3 +1,3 @@
 dependencies:
-  openssl.org: ^0.9
+  openssl.org: ^3
   zlib.net: ^1.2
`)
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	got := res.SkippedProjects["foo.org"]
	if len(got) != 1 || got[0] != "stale.patch" {
		t.Fatalf("SkippedProjects = %v, want foo.org -> [stale.patch]", res.SkippedProjects)
	}
}

// Two patches against the same recipe are both named: the operator has to
// re-cut both, and hearing about one would send them round a second time.
func TestSkippedProjectsCollectsEveryPatch(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	for _, n := range []string{"a-stale.patch", "b-stale.patch"} {
		writeFile(t, dir, n, `--- a/projects/foo.org/package.yml
+++ b/projects/foo.org/package.yml
@@ -1,3 +1,3 @@
 dependencies:
-  openssl.org: ^0.9
+  openssl.org: ^3
   zlib.net: ^1.2
`)
	}
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.SkippedProjects["foo.org"], ","); got != "a-stale.patch,b-stale.patch" {
		t.Fatalf("SkippedProjects[foo.org] = %q", got)
	}
}

// An applied patch names nothing: the map is for what went wrong.
func TestSkippedProjectsEmptyWhenAllApply(t *testing.T) {
	root := initPantry(t)
	dir := t.TempDir()
	writeFile(t, dir, "good.patch", modPatch)
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SkippedProjects) != 0 {
		t.Fatalf("SkippedProjects = %v, want empty", res.SkippedProjects)
	}
}

// A patch that touches something other than a recipe is skipped without naming
// a project — there is no build to refuse.
func TestSkippedProjectsIgnoresNonRecipes(t *testing.T) {
	root := initPantry(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "doc.patch", `--- a/README.md
+++ b/README.md
@@ -1,3 +1,3 @@
 one
-TWO
+two
 three
`)
	res, err := Apply(Options{Dir: dir, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	if len(res.SkippedProjects) != 0 {
		t.Fatalf("SkippedProjects = %v, want empty", res.SkippedProjects)
	}
}

func TestProjectOf(t *testing.T) {
	for _, tc := range []struct {
		path, want string
		ok         bool
	}{
		{"projects/foo.org/package.yml", "foo.org", true},
		{"projects/mozilla.org/nss/package.yml", "mozilla.org/nss", true},
		{"projects/package.yml", "", false},
		{"README.md", "", false},
		{"projects/foo.org/other.yml", "", false},
		{"other/foo.org/package.yml", "", false},
	} {
		got, ok := projectOf(tc.path)
		if got != tc.want || ok != tc.ok {
			t.Errorf("projectOf(%q) = %q,%v; want %q,%v", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}
