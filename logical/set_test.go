package logical

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOverride(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeOverride(t, dir, "curl.se.hcl", `
project = "curl.se"
why     = "our registry carries no openssl 1.x"
edits   = [{ path = "dependencies[\"openssl.org\"]", set = "^3" }]
`)
	writeOverride(t, dir, "notes.txt", "ignored: not an override")

	s, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Projects(); len(got) != 1 || got[0] != "curl.se" {
		t.Fatalf("projects = %v", got)
	}
	if s.For("curl.se") == nil || s.For("absent.example") != nil {
		t.Error("For should answer for what it holds and only that")
	}
	if s.Files["curl.se"] != "curl.se.hcl" {
		t.Errorf("Files = %v — a report has to name the file to open", s.Files)
	}

	doc := map[string]any{"dependencies": map[string]any{"openssl.org": "^1.1"}}
	res, err := s.ApplyTo("curl.se", doc)
	if err != nil || len(res) != 1 || res[0].Outcome != Applied {
		t.Fatalf("%v %v", res, err)
	}
	if doc["dependencies"].(map[string]any)["openssl.org"] != "^3" {
		t.Errorf("got %v", doc)
	}
	// A project the set does not hold is left exactly alone.
	untouched := map[string]any{"a": 1}
	if res, err := s.ApplyTo("absent.example", untouched); err != nil || res != nil {
		t.Errorf("%v %v", res, err)
	}
}

// An empty directory, and no directory at all, are both "no overrides" — during
// the migration most projects are still unified diffs and must keep building.
func TestLoadDirWithNothingInIt(t *testing.T) {
	for _, dir := range []string{"", t.TempDir()} {
		s, err := LoadDir(dir)
		if err != nil || len(s.Projects()) != 0 {
			t.Errorf("%q: %v %v", dir, s, err)
		}
	}
}

// Two files claiming one project would apply in whatever order the glob
// returned — a fact about the filesystem, not about the recipes.
func TestLoadDirRefusesTwoFilesForOneProject(t *testing.T) {
	dir := t.TempDir()
	body := "project = \"p.org\"\nwhy = \"w\"\nedits = [{ path = \"a\", set = 1 }]\n"
	writeOverride(t, dir, "a.hcl", body)
	writeOverride(t, dir, "b.hcl", body)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "both override") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadDirReportsABadFile(t *testing.T) {
	dir := t.TempDir()
	writeOverride(t, dir, "bad.hcl", "project = ")
	if _, err := LoadDir(dir); err == nil {
		t.Error("an override that does not parse must stop the run")
	}
	// A file that cannot be read at all.
	dir2 := t.TempDir()
	p := filepath.Join(dir2, "x.hcl")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir2); err == nil {
		t.Error("an unreadable override must stop the run")
	}
}

// A failure names the FILE, because that is what somebody has to open.
func TestApplyToNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	writeOverride(t, dir, "zip.hcl", `
project = "info-zip.org/zip"
why     = "w"
edits   = [{ path = "build.script", from = "gone", to = "x" }]
`)
	s, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyTo("info-zip.org/zip", map[string]any{"build": map[string]any{"script": []any{"make"}}})
	if err == nil || !strings.Contains(err.Error(), "zip.hcl") {
		t.Fatalf("err = %v", err)
	}
}

// A nil Set answers like an empty one: the callers that have no overrides
// directory pass nil, and none of them should have to check first.
func TestNilSet(t *testing.T) {
	var s *Set
	if s.Projects() != nil || s.For("x") != nil {
		t.Error("a nil set must answer empty")
	}
	if res, err := s.ApplyTo("x", map[string]any{}); err != nil || res != nil {
		t.Errorf("%v %v", res, err)
	}
}

// A directory name that is itself a bad glob pattern: the error is the one
// case Glob reports, and swallowing it would read as "no overrides here".
func TestLoadDirWithAnUnglobbableName(t *testing.T) {
	if _, err := LoadDir("a["); err == nil {
		t.Error("a pattern Glob cannot read must be an error, not an empty set")
	}
}
