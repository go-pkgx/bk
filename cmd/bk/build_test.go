package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bottle"
	"mvdan.cc/sh/v3/interp"
)

const miniRecipe = "distributable: https://x/v{{version.raw}}.tgz\nbuild:\n  script:\n    - make\ntest: \"true\"\n"

func writeRecipe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "package.yml")
	if err := os.WriteFile(p, []byte(miniRecipe), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubFactory returns a factory whose Runner succeeds; its Run creates the
// +brewing dir so the pipeline's rename works. version is fixed to 1.0.0.
func stubFactory(project string) func(string) *build.Runner {
	return func(string) *build.Runner {
		return &build.Runner{
			PickVersion:    func(string, string) (string, error) { return "1.0.0", nil },
			ResolveVersion: func(any, string) (string, string, error) { return "1.0.0", "v1.0.0", nil },
			Fetch:          func(string, string, int) error { return nil },
			FetchGit:       func(string, string, string) error { return nil },
			Touch:          func(string) error { return nil },
			Run: func(string, []string) error {
				return os.MkdirAll(config.Compute(project, "1.0.0", target.Host()).BuildInstall, 0o755)
			},
			FixUp:       func(fixup.Options) error { return nil },
			WriteBottle: func(_, _, _, _, _, out string) (string, error) { return filepath.Join(out, "b.tgz"), nil },
		}
	}
}

func tempEnv(t *testing.T) {
	t.Setenv("PKGX_DIR", filepath.Join(t.TempDir(), "pkgx"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("PKGX_PANTRY_DIR", "")
	t.Setenv("BREWKIT_TARGET", "")
}

func TestBuildCommand(t *testing.T) {
	tempEnv(t)
	old := buildFactory
	defer func() { buildFactory = old }()
	buildFactory = stubFactory("test.org/x")

	rec := writeRecipe(t)
	code, out, errs := run2(t, "build", "--recipe", rec, "--dist", filepath.Join(t.TempDir(), "dist"), "test.org/x")
	if code != 0 {
		t.Fatalf("build: code=%d err=%q", code, errs)
	}
	if !strings.Contains(out, "built test.org/x 1.0.0") || !strings.Contains(out, "bottle:") {
		t.Errorf("build output = %q", out)
	}
}

func TestBuildCommandSetsRecipeDir(t *testing.T) {
	tempEnv(t)
	old := buildFactory
	defer func() { buildFactory = old }()

	var captured *build.Runner
	base := stubFactory("test.org/x")
	buildFactory = func(p string) *build.Runner { captured = base(p); return captured }

	rec := writeRecipe(t)
	code, _, errs := run2(t, "build", "--recipe", rec, "test.org/x")
	if code != 0 {
		t.Fatalf("build: code=%d err=%q", code, errs)
	}
	if captured == nil || captured.RecipeDir != filepath.Dir(rec) {
		t.Errorf("RecipeDir = %q, want %q", captured.RecipeDir, filepath.Dir(rec))
	}
}

func TestBuildCommandErrors(t *testing.T) {
	tempEnv(t)
	old := buildFactory
	defer func() { buildFactory = old }()
	rec := writeRecipe(t)

	// missing --recipe / no project
	if c, _, _ := run2(t, "build"); c != 2 {
		t.Errorf("no-recipe code=%d", c)
	}
	if c, _, _ := run2(t, "build", "--recipe", rec); c != 2 {
		t.Errorf("no-project code=%d", c)
	}
	// bad flag
	if c, _, _ := run2(t, "build", "-nope"); c != 2 {
		t.Errorf("bad-flag code=%d", c)
	}
	// unreadable recipe
	if c, _, _ := run2(t, "build", "--recipe", filepath.Join(t.TempDir(), "nope.yml"), "p"); c != 1 {
		t.Errorf("missing-recipe code=%d", c)
	}
	// invalid recipe content
	bad := filepath.Join(t.TempDir(), "bad.yml")
	os.WriteFile(bad, []byte("build: [oops\n"), 0o644)
	if c, _, _ := run2(t, "build", "--recipe", bad, "p"); c != 1 {
		t.Errorf("bad-recipe code=%d", c)
	}
	// invalid target (BREWKIT_TARGET) → Resolve error. Call runBuild directly:
	// run2 unsets BREWKIT_TARGET for isolation, which would hide this path.
	t.Setenv("BREWKIT_TARGET", "plan9/x86-64")
	var o, e bytes.Buffer
	if c := runBuild([]string{"--recipe", rec, "p"}, &o, &e); c != 1 {
		t.Errorf("bad-target code=%d", c)
	}
	t.Setenv("BREWKIT_TARGET", "")
	// Build error (factory whose PickVersion fails)
	buildFactory = func(string) *build.Runner {
		return &build.Runner{ResolveVersion: func(any, string) (string, string, error) { return "", "", os.ErrInvalid }}
	}
	if c, _, _ := run2(t, "build", "--recipe", rec, "p"); c != 1 {
		t.Errorf("build-error code=%d", c)
	}
}

func TestBuildResolvesPkgxPath(t *testing.T) {
	tempEnv(t)
	oldF, oldL := buildFactory, lookPath
	defer func() { buildFactory, lookPath = oldF, oldL }()

	var gotBin string
	base := stubFactory("test.org/x")
	buildFactory = func(p string) *build.Runner { gotBin = p; return base(p) }
	rec := writeRecipe(t)

	// LookPath succeeds → the absolute path is handed to the factory.
	lookPath = func(name string) (string, error) { return "/abs/" + name, nil }
	if c, _, e := run2(t, "build", "--recipe", rec, "test.org/x"); c != 0 {
		t.Fatalf("build: code=%d err=%q", c, e)
	}
	if gotBin != "/abs/pkgx" {
		t.Errorf("resolved pkgx = %q, want /abs/pkgx", gotBin)
	}

	// LookPath fails → the flag value is kept as given.
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if c, _, e := run2(t, "build", "--recipe", rec, "--pkgx", "/explicit/pkgx", "test.org/x"); c != 0 {
		t.Fatalf("build: code=%d err=%q", c, e)
	}
	if gotBin != "/explicit/pkgx" {
		t.Errorf("unresolved pkgx = %q, want /explicit/pkgx", gotBin)
	}
}

func TestRunBash(t *testing.T) {
	// success: a trivial script runs to completion in the pure-Go interpreter.
	ok := filepath.Join(t.TempDir(), "ok.sh")
	os.WriteFile(ok, []byte("#!/bin/bash\nexport X=1\nif [ \"$X\" = 1 ]; then :; fi\nexit 0\n"), 0o755)
	if err := runBash(ok, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Errorf("runBash ok: %v", err)
	}
	// non-zero exit → the interpreter returns an error.
	bad := filepath.Join(t.TempDir(), "bad.sh")
	os.WriteFile(bad, []byte("#!/bin/bash\nexit 3\n"), 0o755)
	if err := runBash(bad, nil); err == nil {
		t.Error("runBash should error on exit 3")
	}
	// open error: a path that does not exist.
	if err := runBash(filepath.Join(t.TempDir(), "nope.sh"), nil); err == nil {
		t.Error("runBash should error on a missing script")
	}
	// parse error: malformed shell syntax.
	mal := filepath.Join(t.TempDir(), "mal.sh")
	os.WriteFile(mal, []byte("if then fi\n"), 0o755)
	if err := runBash(mal, nil); err == nil {
		t.Error("runBash should error on a parse failure")
	}
	// interpreter-constructor error: forced via the seam.
	origNew := newInterp
	newInterp = func(...interp.RunnerOption) (*interp.Runner, error) { return nil, errBoom }
	defer func() { newInterp = origNew }()
	if err := runBash(ok, nil); err == nil {
		t.Error("runBash should propagate an interp.New error")
	}
}

func TestPickVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/versions.txt") {
			w.Write([]byte("1.0.0\n2.3.4\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	sd := bottle.DistBase
	bottle.DistBase = srv.URL
	defer func() { bottle.DistBase = sd }()

	v, err := pickVersion("acme.org/tool", "*")
	if err != nil || v != "2.3.4" {
		t.Errorf("pickVersion = %q %v", v, err)
	}
}

func TestRealBuildRunner(t *testing.T) {
	r := realBuildRunner("/opt/pkgx/bin/pkgx")
	if r.PickVersion == nil || r.ResolveVersion == nil || r.Fetch == nil || r.FetchGit == nil || r.Touch == nil ||
		r.Run == nil || r.FixUp == nil || r.WriteBottle == nil || r.ResolveDep == nil || r.PkgxBin == "" || r.BashPath == "" {
		t.Errorf("realBuildRunner not fully wired: %+v", r)
	}
}
