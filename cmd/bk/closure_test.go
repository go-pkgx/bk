package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/recipefile"
	"github.com/go-pkgx/bk/target"
)

// writeClosureRecipe drops a package.yml under <pantry>/projects/<proj>/.
func writeClosureRecipe(t *testing.T, pantry, proj, yaml string) {
	t.Helper()
	dir := filepath.Join(pantry, "projects", proj)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunClosure(t *testing.T) {
	p := t.TempDir()
	// app -> lib -> (nothing); app also -> missing.org (no recipe → skipped)
	writeClosureRecipe(t, p, "app.org", "dependencies:\n  lib.org: '*'\n  missing.org: '*'\nversions:\n  github: a/app/tags\nbuild: make\n")
	writeClosureRecipe(t, p, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")

	var out, errb bytes.Buffer
	if code := runClosure([]string{"--pantry", p, "--platform", "linux/x86-64", "app.org"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	lines := strings.Fields(out.String())
	// topological: lib.org before app.org; missing.org absent (no recipe).
	if len(lines) != 2 || lines[0] != "lib.org" || lines[1] != "app.org" {
		t.Errorf("order = %v, want [lib.org app.org]", lines)
	}
	if !strings.Contains(errb.String(), "skip missing.org") {
		t.Errorf("expected skip note for missing.org, got %q", errb.String())
	}
	// idempotent revisit: depending on app twice must not duplicate.
	out.Reset()
	errb.Reset()
	runClosure([]string{"--pantry", p, "app.org", "app.org"}, &out, &errb)
	if n := len(strings.Fields(out.String())); n != 2 {
		t.Errorf("cycle/dup guard: got %d lines, want 2", n)
	}
}

func TestRunClosureFlagError(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runClosure([]string{"-nope"}, &out, &errb); code != 2 {
		t.Errorf("bad flag code=%d, want 2", code)
	}
}

func TestRunClosureEnvFallback(t *testing.T) {
	p := t.TempDir()
	writeClosureRecipe(t, p, "leaf.org", "versions:\n  github: a/leaf/tags\nbuild: make\n")
	t.Setenv("PANTRY", p)
	t.Setenv("PLATFORM", "linux/aarch64")
	var out, errb bytes.Buffer
	if code := runClosure([]string{"leaf.org"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "leaf.org" {
		t.Errorf("env-fallback out = %q", out.String())
	}
}

func TestDepName(t *testing.T) {
	for spec, want := range map[string]string{
		"openssl.org@^1.1":               "openssl.org",
		"zlib.net":                       "zlib.net",
		"invisible-island.net/ncurses^6": "invisible-island.net/ncurses",
		"cmake.org~3.30":                 "cmake.org",
		"gnu.org/gmp>=6":                 "gnu.org/gmp",
		"llvm.org<19":                    "llvm.org",
		"zlib.net=1.3.1":                 "zlib.net",
	} {
		if got := depName(spec); got != want {
			t.Errorf("depName(%q)=%q want %q", spec, got, want)
		}
	}
}

// TestRunClosureRangeConstrainedDep pins the defect that hid gnu.org/readline's
// ncurses: DepSpecs renders `dependencies: {invisible-island.net/ncurses: ^6}`
// as "…/ncurses^6", and a closure that splits on "@" alone looked for a project
// literally named "…/ncurses^6", skipped it, and built readline with no ncurses
// in scope — `ld.lld: unable to find library -lncursesw`, three layers away.
func TestRunClosureRangeConstrainedDep(t *testing.T) {
	p := t.TempDir()
	writeClosureRecipe(t, p, "app.org", "dependencies:\n  lib.org: ^6\nversions:\n  github: a/app/tags\nbuild: make\n")
	writeClosureRecipe(t, p, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")

	var out, errb bytes.Buffer
	if code := runClosure([]string{"--pantry", p, "--platform", "linux/aarch64", "app.org"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	if lines := strings.Fields(out.String()); len(lines) != 2 || lines[0] != "lib.org" || lines[1] != "app.org" {
		t.Errorf("order = %v, want [lib.org app.org]", lines)
	}
	if errb.Len() != 0 {
		t.Errorf("a caret-constrained dep must resolve, not be skipped: %q", errb.String())
	}
}

// TestClosureDispatch covers the main-loop `case "closure"` route.
func TestClosureDispatch(t *testing.T) {
	p := t.TempDir()
	writeClosureRecipe(t, p, "leaf.org", "versions:\n  github: a/leaf/tags\nbuild: make\n")
	code, out, errs := run2(t, "closure", "--pantry", p, "leaf.org")
	if code != 0 {
		t.Fatalf("dispatch closure code=%d err=%q", code, errs)
	}
	if strings.TrimSpace(out) != "leaf.org" {
		t.Errorf("dispatch closure out=%q", out)
	}
}

// writeClosureRecipeNamed writes a recipe under a chosen filename, for tests
// that care which front-end reads it.
func writeClosureRecipeNamed(t *testing.T, pantry, proj, name, body string) {
	t.Helper()
	dir := filepath.Join(pantry, "projects", proj)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The edge that started this: perl.org NEEDs libcrypt.so.1, our overlay says
// so and upstream's recipe does not. The factory's own walk read the pantry
// alone, so the provider was never built — while every consumer resolved it
// from the overlay and found nothing in the registry.
func TestClosureOfSeesAnOverlayOnlyDependency(t *testing.T) {
	pantry, overlay := t.TempDir(), t.TempDir()
	writeClosureRecipe(t, pantry, "perl.org", "dependencies:\n  gnu.org/gdbm: '*'\n")
	writeClosureRecipe(t, overlay, "perl.org", "dependencies:\n  github.com/besser82/libxcrypt: '*'\n")
	writeClosureRecipe(t, pantry, "gnu.org/gdbm", "build: make\n")
	writeClosureRecipe(t, pantry, "github.com/besser82/libxcrypt", "build: make\n")

	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	order, _ := closureOf(overlay, pantry, tgt, []string{"perl.org"}, func(string) {})
	for _, want := range []string{"github.com/besser82/libxcrypt", "gnu.org/gdbm", "perl.org"} {
		if !slices.Contains(order, want) {
			t.Errorf("order = %v, missing %s", order, want)
		}
	}
	// Both halves, not the overlay winning: the pantry's own dependency is
	// what the BUILD will need in its environment.
	if len(order) != 3 {
		t.Errorf("order = %v, want exactly the three", order)
	}

	// Without the overlay this is the walk it replaces, blind spot and all.
	if order, _ := closureOf("", pantry, tgt, []string{"perl.org"}, func(string) {}); slices.Contains(order, "github.com/besser82/libxcrypt") {
		t.Errorf("a pantry-only walk cannot see the overlay's edge: %v", order)
	}
}

// A constraint in either half is a constraint something will ask for:
// surrealdb.com asks rust-lang.org for ">=1.60" in the overlay and "~1.95" in
// the patched pantry, and a version satisfying only one of them leaves the
// other asking for something nothing built.
func TestClosureOfCollectsDemandsFromBothHalves(t *testing.T) {
	pantry, overlay := t.TempDir(), t.TempDir()
	writeClosureRecipe(t, pantry, "app.org", "dependencies:\n  lib.org: ~1.95\n")
	writeClosureRecipe(t, overlay, "app.org", "dependencies:\n  lib.org: '>=1.60'\n")
	writeClosureRecipe(t, pantry, "lib.org", "build: make\n")

	_, demands := closureOf(overlay, pantry, target.Target{Platform: "linux", Arch: "x86-64"},
		[]string{"app.org"}, func(string) {})
	got := demands["lib.org"]
	slices.Sort(got)
	if len(got) != 2 || got[0] != ">=1.60" || got[1] != "~1.95" {
		t.Errorf("demands = %v, want both halves", got)
	}
}

// A project only the overlay carries is still a project, and a recipe that
// exists in either half and does not parse is never skipped quietly: the walk
// would plan a build around a recipe it could not read.
func TestClosureRecipes(t *testing.T) {
	pantry, overlay := t.TempDir(), t.TempDir()
	writeClosureRecipe(t, overlay, "only.example", "build: make\n")
	if recs, err := closureRecipes(overlay, pantry, "only.example"); err != nil || len(recs) != 1 {
		t.Errorf("overlay-only: %d recipe(s), %v", len(recs), err)
	}
	if _, err := closureRecipes(overlay, pantry, "absent.example"); !errors.Is(err, recipefile.ErrNoRecipe) {
		t.Errorf("absent everywhere: %v", err)
	}
	writeClosureRecipe(t, overlay, "broken.example", "dependencies: [\n")
	if _, err := closureRecipes(overlay, pantry, "broken.example"); err == nil || errors.Is(err, recipefile.ErrNoRecipe) {
		t.Errorf("a recipe that exists and does not parse must be reported: %v", err)
	}
	// With no overlay it is the pantry-only loader it replaces.
	writeClosureRecipe(t, pantry, "plain.example", "build: make\n")
	if recs, err := closureRecipes("", pantry, "plain.example"); err != nil || len(recs) != 1 {
		t.Errorf("pantry-only: %d recipe(s), %v", len(recs), err)
	}
}
