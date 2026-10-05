package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgx/bottle"
)

// catbed is a pantry and an overlay with a few recipes in each, plus the
// frozen clock and captured write the other command tests use.
func catbed(t *testing.T, pantry, overlay map[string]string) (string, string, *map[string][]byte) {
	t.Helper()
	write := func(root string, recipes map[string]string) string {
		dir := t.TempDir()
		for proj, body := range recipes {
			p := filepath.Join(dir, "projects", proj)
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "package.yml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	p := write("pantry", pantry)
	o := ""
	if overlay != nil {
		o = write("overlay", overlay)
	}
	prevNow, prevWrite := catalogNow, osWriteFile
	catalogNow = func() time.Time { return time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) }
	files := map[string][]byte{}
	osWriteFile = func(name string, b []byte, _ os.FileMode) error { files[name] = b; return nil }
	t.Cleanup(func() { catalogNow, osWriteFile = prevNow, prevWrite })
	return p, o, &files
}

const catRecipe = "versions:\n  - 1.0.0\nbuild: make\n"

// The UNION of pantry and overlay, because both are real: a project in only
// one of them is still a project somebody can ask for.
func TestCatalogIsTheUnionOfPantryAndOverlay(t *testing.T) {
	p, o, _ := catbed(t,
		map[string]string{"zlib.net": catRecipe, "gnu.org/bash": catRecipe},
		map[string]string{"gnu.org/bash": catRecipe, "curl.se/ca-certs": catRecipe})
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--overlay", o}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	cat, err := bottle.UnmarshalCatalog(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pr := range cat.Projects {
		names = append(names, pr.Project)
	}
	if strings.Join(names, " ") != "curl.se/ca-certs gnu.org/bash zlib.net" {
		t.Errorf("projects = %v (the union, sorted, deduplicated)", names)
	}
	if cat.Generated != "2026-10-05T09:00:00Z" {
		t.Errorf("generated = %q", cat.Generated)
	}
}

// An empty catalogue would be published and believed. A pantry that reads as
// empty is a mistyped path far more often than an empty pantry.
func TestCatalogRefusesToBuildNothing(t *testing.T) {
	// A pantry that EXISTS and holds no recipe. Not a bare temp dir: a
	// directory with no `projects/` at all is unreadable, not empty, and
	// the next assertion is precisely that the two differ.
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", empty}, &out, &errb); code != 1 {
		t.Fatalf("an empty pantry exited %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not a result") {
		t.Errorf("stderr=%q", errb.String())
	}
	// A pantry path that does not exist is a DIFFERENT failure and must say
	// so. Both exit 1; only the message tells the user which mistake they
	// made, and for a while both said "an empty catalogue is not a result".
	errb.Reset()
	missing := filepath.Join(t.TempDir(), "nope")
	if code := runCatalog([]string{"--pantry", missing}, &out, &errb); code != 1 {
		t.Errorf("an absent pantry exited %d", code)
	}
	if strings.Contains(errb.String(), "not a result") {
		t.Errorf("an unreadable pantry was reported as an empty one: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "no such file") {
		t.Errorf("it does not say what went wrong: %q", errb.String())
	}
}

// A file that is not a recipe is skipped rather than taken for a project.
func TestCatalogIgnoresWhatIsNotARecipe(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"zlib.net": catRecipe}, nil)
	for _, name := range []string{"README.md", "test.c"} {
		if err := os.WriteFile(filepath.Join(p, "projects", "zlib.net", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	cat, _ := bottle.UnmarshalCatalog(out.Bytes())
	if len(cat.Projects) != 1 || cat.Projects[0].Project != "zlib.net" {
		t.Errorf("projects = %+v", cat.Projects)
	}
}

func TestCatalogRefusesABadPlatformAndBadFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--platform", "linux"}, &out, &errb); code != 2 {
		t.Errorf("a platform with no slash exited %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "wants os/arch") {
		t.Errorf("stderr=%q", errb.String())
	}
	if code := runCatalog([]string{"--nope"}, &out, &errb); code != 2 {
		t.Errorf("an unknown flag exited %d, want 2", code)
	}
}

func TestCatalogWritesAFile(t *testing.T) {
	p, _, files := catbed(t, map[string]string{"zlib.net": catRecipe}, nil)
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "-o", "/tmp/c.json"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	if _, ok := (*files)["/tmp/c.json"]; !ok {
		t.Fatalf("nothing written; %v", *files)
	}
	if out.Len() != 0 {
		t.Errorf("it wrote the file AND stdout: %q", out.String())
	}
	if !strings.Contains(errb.String(), "1 project(s)") {
		t.Errorf("no count: %q", errb.String())
	}
	// A write that fails is a failure.
	prev := osWriteFile
	osWriteFile = func(string, []byte, os.FileMode) error { return os.ErrPermission }
	t.Cleanup(func() { osWriteFile = prev })
	if code := runCatalog([]string{"--pantry", p, "-o", "/tmp/c.json"}, &out, &errb); code != 1 {
		t.Errorf("an unwritable file exited %d, want 1", code)
	}
}

// A catalogue that cannot be rendered is not published as a shorter one.
func TestCatalogReportsAMarshalFailure(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"zlib.net": catRecipe}, nil)
	prev := marshalCatalog
	marshalCatalog = func(bottle.Catalog) ([]byte, error) { return nil, errors.New("nope") }
	t.Cleanup(func() { marshalCatalog = prev })
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p}, &out, &errb); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errb.String(), "nope") {
		t.Errorf("stderr=%q", errb.String())
	}
}

func TestCatalogPublishes(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"zlib.net": catRecipe, "gnu.org/bash": catRecipe}, nil)
	var gotOS, gotArch string
	var gotCat bottle.Catalog
	prevP, prevC := publishCatalog, catalogClient
	publishCatalog = func(_ *bottle.OCIClient, c bottle.Catalog, osn, arch string) error {
		gotCat, gotOS, gotArch = c, osn, arch
		return nil
	}
	catalogClient = func(string) (*bottle.OCIClient, error) { return nil, nil }
	t.Cleanup(func() { publishCatalog, catalogClient = prevP, prevC })

	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--platform", "linux/s390x", "--publish"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	if gotOS != "linux" || gotArch != "s390x" {
		t.Errorf("published for %s/%s", gotOS, gotArch)
	}
	if len(gotCat.Projects) != 2 {
		t.Errorf("published %d projects", len(gotCat.Projects))
	}
	if out.Len() != 0 {
		t.Errorf("a publish also wrote the body to stdout: %q", out.String())
	}

	// Both ways it can fail.
	publishCatalog = func(*bottle.OCIClient, bottle.Catalog, string, string) error { return errors.New("registry down") }
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 1 {
		t.Errorf("a failed push exited %d", code)
	}
	catalogClient = func(string) (*bottle.OCIClient, error) { return nil, errors.New("bad dist") }
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 1 {
		t.Errorf("an unusable dist exited %d", code)
	}
}

// --versions is one request per project, so it SAYS how many before making
// them, and a project the registry cannot answer for is warned rather than
// taking the other 1907 down with it.
func TestCatalogVersionsIsOptInAndSurvivesOneFailure(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"a.org": catRecipe, "b.org": catRecipe}, nil)
	prev := versionsFor
	versionsFor = func(project, osn, arch string) ([]bottle.Ver, error) {
		switch project {
		case "a.org":
			return []bottle.Ver{bottle.ParseVer("1.0.0"), bottle.ParseVer("2.0.0")}, nil
		case "b.org":
			return nil, errors.New("not found")
		}
		return nil, nil
	}
	t.Cleanup(func() { versionsFor = prev })

	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--versions"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "asking the registry about 2 project(s)") {
		t.Errorf("it did not say how many requests it would make: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "b.org: not found") {
		t.Errorf("the one failure was not warned: %q", errb.String())
	}
	cat, err := bottle.UnmarshalCatalog(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := cat.Lookup("a.org")
	if len(a.Versions) != 2 || a.Versions[0] != "2.0.0" {
		t.Errorf("a.org = %+v (newest first)", a)
	}
	b, _ := cat.Lookup("b.org")
	if len(b.Versions) != 0 {
		t.Errorf("b.org got versions from a failed call: %+v", b)
	}
	// And without --versions, nothing is asked at all.
	versionsFor = func(string, string, string) ([]bottle.Ver, error) {
		t.Fatal("the registry was asked without --versions")
		return nil, nil
	}
	out.Reset()
	if code := runCatalog([]string{"--pantry", p}, &out, &errb); code != 0 {
		t.Fatalf("code=%d", code)
	}
}

// A project whose registry entry is empty keeps an empty Versions rather
// than an invented one.
func TestCatalogVersionsSkipsAnEmptyAnswer(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"a.org": catRecipe}, nil)
	prev := versionsFor
	versionsFor = func(string, string, string) ([]bottle.Ver, error) { return nil, nil }
	t.Cleanup(func() { versionsFor = prev })
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--versions"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	cat, _ := bottle.UnmarshalCatalog(out.Bytes())
	if a, _ := cat.Lookup("a.org"); len(a.Versions) != 0 || len(a.Platforms) != 0 {
		t.Errorf("an empty answer invented something: %+v", a)
	}
}

// TestCatalogDispatch covers the main-loop route.
func TestCatalogDispatch(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"zlib.net": catRecipe}, nil)
	code, out, errs := run2(t, "catalog", "--pantry", p)
	if code != 0 {
		t.Fatalf("code=%d %s", code, errs)
	}
	if !strings.Contains(out, `"zlib.net"`) {
		t.Errorf("out=%q", out)
	}
}

// One unreadable directory inside the pantry must not take the catalogue
// down with it: the other 1907 projects are still projects. Only a failure
// AT THE ROOT means the pantry could not be read.
func TestCatalogSurvivesOneUnreadableDirectory(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"zlib.net": catRecipe, "gnu.org/bash": catRecipe}, nil)
	locked := filepath.Join(p, "projects", "locked.org")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skip("cannot make a directory unreadable here:", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if f, err := os.Open(locked); err == nil { // running as root, say
		_ = f.Close()
		t.Skip("this user can read a 0000 directory")
	}

	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p}, &out, &errb); code != 0 {
		t.Fatalf("one unreadable directory failed the whole catalogue: %d %s", code, errb.String())
	}
	cat, err := bottle.UnmarshalCatalog(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Projects) != 2 {
		t.Errorf("projects = %+v", cat.Projects)
	}
}
