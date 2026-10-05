package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-attest/sbom"
	"github.com/go-attest/sign"
	"github.com/go-pkgx/bk/logical"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bottle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

// A catalogue is published the way a bottle is published, SIGNATURE
// INCLUDED. A client with PKGX_VERIFY on — the default — refuses an
// unsigned one, and rightly: the catalogue is the list of names a person
// then types, so deciding its contents is deciding what <TAB> offers.
func TestCatalogPublishes(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"zlib.net": catRecipe, "gnu.org/bash": catRecipe}, nil)
	var gotOS, gotArch, gotProject, gotVer, gotExt string
	var gotTgz []byte
	var gotRefs []bottle.Referrer
	prevP, prevK, prevT := ociPush, signingKey, catalogTarball
	ociPush = func(_, project, ver, osn, arch string, tgz []byte, ext string, refs []bottle.Referrer, _ map[string]string) (ocispec.Descriptor, error) {
		gotProject, gotVer, gotOS, gotArch, gotTgz, gotExt, gotRefs = project, ver, osn, arch, tgz, ext, refs
		return ocispec.Descriptor{}, nil
	}
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	signingKey = func(string) (*sign.Keypair, error) { return kp, nil }
	t.Cleanup(func() { ociPush, signingKey, catalogTarball = prevP, prevK, prevT })

	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--platform", "linux/s390x", "--publish"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	if gotOS != "linux" || gotArch != "s390x" {
		t.Errorf("published for %s/%s", gotOS, gotArch)
	}
	if gotProject != bottle.CatalogProjectName || gotVer != bottle.CatalogVersion || gotExt != bottle.ExtTarGz {
		t.Errorf("published as %s v%s %s", gotProject, gotVer, gotExt)
	}
	cat, err := bottle.CatalogFromTarball(gotTgz)
	if err != nil {
		t.Fatalf("what was pushed is not a catalogue: %v", err)
	}
	if len(cat.Projects) != 2 {
		t.Errorf("published %d projects", len(cat.Projects))
	}
	// The signature, which is the point.
	signed := false
	for _, r := range gotRefs {
		if r.ArtifactType == bottle.ArtifactTypeSignature {
			signed = true
		}
	}
	if !signed {
		t.Error("the catalogue went out unsigned — a client with PKGX_VERIFY on refuses it")
	}
	if !strings.Contains(errb.String(), "+signature") {
		t.Errorf("the publish does not say it signed: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("a publish also wrote the body to stdout: %q", out.String())
	}

	// No key: it still publishes, because the sovereign seed registry has
	// none and PKGX_VERIFY=0 there — but it says what that means, in terms
	// of what a client will do, not as advice about style.
	errb.Reset()
	signingKey = func(string) (*sign.Keypair, error) { return nil, nil }
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 0 {
		t.Fatalf("an unsigned publish exited %d: %s", code, errb.String())
	}
	for _, r := range gotRefs {
		if r.ArtifactType == bottle.ArtifactTypeSignature {
			t.Error("a signature appeared with no key")
		}
	}
	if !strings.Contains(errb.String(), "PKGX_VERIFY") {
		t.Errorf("an unsigned publish does not say what a client will do: %q", errb.String())
	}

	// Every way it can fail, because a publish that reports success on a
	// registry that refused it is how an empty catalogue gets believed.
	signingKey = func(string) (*sign.Keypair, error) { return nil, errors.New("not a key") }
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 1 {
		t.Errorf("an unreadable key exited %d", code)
	}
	signingKey = func(string) (*sign.Keypair, error) { return nil, nil }

	catalogTarball = func(bottle.Catalog) ([]byte, error) { return nil, errors.New("cannot pack") }
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 1 {
		t.Errorf("an unpackable catalogue exited %d", code)
	}
	catalogTarball = prevT

	// A referrer that cannot be built: the attestations are not optional
	// decoration, they are what the thing is published WITH.
	prevSbom := sbomJSON
	sbomJSON = func(sbom.Document) ([]byte, error) { return nil, errors.New("no sbom") }
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 1 {
		t.Errorf("an unbuildable referrer exited %d", code)
	}
	sbomJSON = prevSbom

	ociPush = func(string, string, string, string, string, []byte, string, []bottle.Referrer, map[string]string) (ocispec.Descriptor, error) {
		return ocispec.Descriptor{}, errors.New("registry down")
	}
	if code := runCatalog([]string{"--pantry", p, "--publish"}, &out, &errb); code != 1 {
		t.Errorf("a failed push exited %d", code)
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

// The dependency half of the catalogue: what a project NEEDS, reduced for
// the platform, read off the recipe already on disk.
func TestCatalogCarriesRuntimeDependencies(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{
		"app.org": "versions:\n  - 1.0.0\ndependencies:\n  lib.org: '*'\n  linux:\n    only-on-linux.org: '*'\nbuild:\n  dependencies:\n    buildonly.org: '*'\n  script: make\n",
		"lib.org": catRecipe,
	}, nil)
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--platform", "linux/x86-64"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	cat, err := bottle.UnmarshalCatalog(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	app, _ := cat.Lookup("app.org")
	if strings.Join(app.Deps, " ") != "lib.org only-on-linux.org" {
		t.Errorf("app.org deps = %v (sorted, platform-reduced)", app.Deps)
	}
	// BUILD dependencies are not in anybody's installed closure, so they
	// are not under the node a browser is looking at.
	for _, d := range app.Deps {
		if d == "buildonly.org" {
			t.Error("a build dependency reached the catalogue")
		}
	}
	// And the other platform's dependency is NOT there.
	out.Reset()
	if code := runCatalog([]string{"--pantry", p, "--platform", "darwin/aarch64"}, &out, &errb); code != 0 {
		t.Fatalf("darwin: code=%d", code)
	}
	dcat, _ := bottle.UnmarshalCatalog(out.Bytes())
	dapp, _ := dcat.Lookup("app.org")
	if strings.Join(dapp.Deps, " ") != "lib.org" {
		t.Errorf("darwin app.org deps = %v; the linux-only one leaked", dapp.Deps)
	}
}

// A project the walk NAMED and the reader cannot load is COUNTED, not
// silent. Listing it with no dependencies is a statement, and a false one.
func TestCatalogCountsProjectsItCannotRead(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"a.org": catRecipe, "b.org": catRecipe}, nil)
	prev := recipeLoader
	recipeLoader = func(set *logical.Set, overlay, pantry, proj string) (*pantry.Recipe, string, error) {
		if proj == "b.org" {
			return nil, "", errors.New("vanished")
		}
		return prev(set, overlay, pantry, proj)
	}
	t.Cleanup(func() { recipeLoader = prev })

	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p}, &out, &errb); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errb.String(), "1 project(s) named but unreadable") {
		t.Errorf("the unreadable project was not counted: %q", errb.String())
	}
	// It is still LISTED — it exists, we just cannot say what it needs.
	if _, ok := (func() (bottle.CatalogProject, bool) {
		c, _ := bottle.UnmarshalCatalog(out.Bytes())
		return c.Lookup("b.org")
	}()); !ok {
		t.Error("an unreadable project was dropped from the catalogue entirely")
	}
}

// An unparseable --overrides is a startup error here too: the overrides
// change what a recipe DEPENDS ON, so a catalogue built without them would
// describe a different tree from the one the factory builds.
func TestCatalogRefusesUnparseableOverrides(t *testing.T) {
	p, _, _ := catbed(t, map[string]string{"a.org": catRecipe}, nil)
	ov := t.TempDir()
	if err := os.WriteFile(filepath.Join(ov, "broken.hcl"), []byte("project \"x\" {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runCatalog([]string{"--pantry", p, "--overrides", ov}, &out, &errb); code != 2 {
		t.Fatalf("code=%d, want 2 (stderr=%q)", code, errb.String())
	}
}
