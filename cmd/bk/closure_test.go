package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
