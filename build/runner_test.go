package build

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
)

var errBoom = errors.New("boom")

// tenv points config at temp dirs so no real PKGX_DIR/data-home is touched.
func tenv(t *testing.T) {
	t.Setenv("PKGX_DIR", filepath.Join(t.TempDir(), "pkgx"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("PKGX_PANTRY_DIR", "")
	t.Setenv("PKGX_PANTRY_PATH", "")
}

func okRecipe() *pantry.Recipe {
	return &pantry.Recipe{
		Distributable: map[string]any{"url": "https://x/v{{version.raw}}.tar.gz", "strip-components": 1},
		Dependencies:  map[string]any{"openssl.org": "^1.1"},
		Build:         map[string]any{"script": []any{"make install"}, "dependencies": map[string]any{"freedesktop.org/pkg-config": "^0.29"}},
	}
}

// okRunner returns a Runner whose stubs all succeed; its Run stub creates the
// +brewing install dir so the subsequent rename works.
func okRunner(project string, tgt target.Target) *Runner {
	return &Runner{
		PickVersion:    func(string, string) (string, error) { return "1.2.3", nil },
		ResolveVersion: func(any, string) (string, error) { return "1.2.3", nil },
		Fetch:          func(string, string, int) error { return nil },
		FetchGit:       func(string, string, string) error { return nil },
		Touch:          func(string) error { return nil },
		Run: func(string, []string) error {
			p := config.Compute(project, "1.2.3", tgt)
			return os.MkdirAll(p.BuildInstall, 0o755)
		},
		FixUp:       func(fixup.Options) error { return nil },
		WriteBottle: func(_, _, _, _, _, out string) (string, error) { return filepath.Join(out, "b.tar.gz"), nil },
		PkgxBin:     "/pkgx/bin/pkgx", BashPath: "/bin/bash",
	}
}

func TestBuildHappyPath(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	r := okRunner("acme.org/tool", tgt)
	res, err := r.Build(okRecipe(), "acme.org/tool", "*", tgt, tgt, filepath.Join(t.TempDir(), "dist"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.2.3" || res.BottlePath == "" || res.ScriptPath == "" {
		t.Errorf("result = %+v", res)
	}
	b, _ := os.ReadFile(res.ScriptPath)
	s := string(b)
	// the script wires deps + base toolchain + the user script + a cd to srcroot
	if !strings.Contains(s, `"+openssl.org@^1.1"`) || !strings.Contains(s, `"+gnu.org/autoconf"`) || !strings.Contains(s, "make install") {
		t.Errorf("script missing pieces:\n%s", s)
	}
	// no dist → no bottle
	r2 := okRunner("acme.org/tool", tgt)
	res2, err := r2.Build(okRecipe(), "acme.org/tool", "*", tgt, tgt, "")
	if err != nil || res2.BottlePath != "" {
		t.Errorf("no-dist: %+v %v", res2, err)
	}
}

func TestBuildWithResolveDep(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	r := okRunner("acme.org/tool", tgt)
	r.ResolveDep = func(string, string) (string, error) { return "9.9.9", nil }
	rec := okRecipe()
	// a build script that references {{deps.<project>.prefix}} so the resolved
	// token actually expands into the generated script
	rec.Build = map[string]any{"script": []any{"./configure --with-ssl={{deps.openssl.org.prefix}}"}}
	res, err := r.Build(rec, "acme.org/tool", "*", tgt, tgt, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.ScriptPath)
	if !strings.Contains(string(b), "openssl.org/v9.9.9") {
		t.Errorf("resolved dep prefix not expanded in script:\n%s", b)
	}
}

func TestBuildGitAndNoDistributable(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	// git distributable
	rec := okRecipe()
	rec.Distributable = map[string]any{"url": "https://git/repo", "ref": "v{{version.raw}}"}
	r := okRunner("g/p", tgt)
	gitCalled := false
	r.FetchGit = func(_, ref, _ string) error {
		gitCalled = true
		if ref != "v1.2.3" {
			t.Errorf("git ref = %q", ref)
		}
		return nil
	}
	if _, err := r.Build(rec, "g/p", "*", tgt, tgt, ""); err != nil || !gitCalled {
		t.Errorf("git build: %v called=%v", err, gitCalled)
	}
	// nil distributable → fetch skipped entirely
	rec2 := okRecipe()
	rec2.Distributable = nil
	r2 := okRunner("n/p", tgt)
	r2.Fetch = func(string, string, int) error { t.Fatal("fetch must not run"); return nil }
	if _, err := r2.Build(rec2, "n/p", "*", tgt, tgt, ""); err != nil {
		t.Fatal(err)
	}
}

func TestBuildErrorBranches(t *testing.T) {
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	restore := func() {
		osRemoveAll, osMkdirAll, osWriteFile, osRename = os.RemoveAll, os.MkdirAll, os.WriteFile, os.Rename
	}

	cases := map[string]func(r *Runner){
		"resolveversion": func(r *Runner) { r.ResolveVersion = func(any, string) (string, error) { return "", errBoom } },
		"removeall":      func(r *Runner) { osRemoveAll = func(string) error { return errBoom } },
		"mkdirall":       func(r *Runner) { osMkdirAll = func(string, os.FileMode) error { return errBoom } },
		"mkdir-home": func(r *Runner) {
			n := 0
			osMkdirAll = func(p string, m os.FileMode) error {
				if n++; n >= 2 {
					return errBoom
				}
				return os.MkdirAll(p, m)
			}
		},
		"fetch":       func(r *Runner) { r.Fetch = func(string, string, int) error { return errBoom } },
		"touch":       func(r *Runner) { r.Touch = func(string) error { return errBoom } },
		"writefile":   func(r *Runner) { osWriteFile = func(string, []byte, os.FileMode) error { return errBoom } },
		"run":         func(r *Runner) { r.Run = func(string, []string) error { return errBoom } },
		"rename":      func(r *Runner) { osRename = func(string, string) error { return errBoom } },
		"fixup":       func(r *Runner) { r.FixUp = func(fixup.Options) error { return errBoom } },
		"writebottle": func(r *Runner) { r.WriteBottle = func(_, _, _, _, _, _ string) (string, error) { return "", errBoom } },
		"resolvedep":  func(r *Runner) { r.ResolveDep = func(string, string) (string, error) { return "", errBoom } },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			tenv(t)
			defer restore()
			r := okRunner("acme.org/tool", tgt)
			mut(r)
			if _, err := r.Build(okRecipe(), "acme.org/tool", "*", tgt, tgt, filepath.Join(t.TempDir(), "dist")); err == nil {
				t.Errorf("%s: expected error", name)
			}
		})
	}
}

func TestBuildSourceAndGenerateErrors(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	// bad distributable type → sourceOf error
	r := okRunner("p", tgt)
	rec := okRecipe()
	rec.Distributable = 123
	if _, err := r.Build(rec, "p", "*", tgt, tgt, ""); err == nil {
		t.Error("expected sourceOf error")
	}
	// build script with a bad step → Generate error
	rec2 := okRecipe()
	rec2.Distributable = "https://x/v{{version.raw}}.tgz"
	rec2.Build = map[string]any{"script": []any{map[string]any{"run": 5}}}
	if _, err := r.Build(rec2, "p", "*", tgt, tgt, ""); err == nil {
		t.Error("expected Generate error")
	}
}

func TestSourceOf(t *testing.T) {
	if s, _ := sourceOf("https://x/v{{version.raw}}.tgz", "1.2.3"); s.url != "https://x/v1.2.3.tgz" || s.git {
		t.Errorf("string dist = %+v", s)
	}
	s, err := sourceOf(map[string]any{"url": "u-{{version.major}}", "strip-components": "2"}, "3.4.5")
	if err != nil || s.url != "u-3" || s.strip != 2 {
		t.Errorf("map dist = %+v %v", s, err)
	}
	// git via ref + int strip
	s, _ = sourceOf(map[string]any{"url": "g", "ref": "r{{version.minor}}", "strip-components": 1}, "3.4.5")
	if !s.git || s.ref != "r4" || s.strip != 1 {
		t.Errorf("git dist = %+v", s)
	}
	// missing url + unsupported type
	if _, err := sourceOf(map[string]any{}, "1"); err == nil {
		t.Error("expected missing-url error")
	}
	if _, err := sourceOf(42, "1"); err == nil {
		t.Error("expected unsupported-type error")
	}
}

func TestHelpers(t *testing.T) {
	if (&Runner{Concurrency: 3}).concurrency() != 3 {
		t.Error("explicit concurrency")
	}
	if (&Runner{}).concurrency() < 1 {
		t.Error("default concurrency")
	}
	if buildDeps(&pantry.Recipe{Build: "just a string"}) != nil {
		t.Error("non-map build → nil deps")
	}
	if str(5) != "" || str("x") != "x" {
		t.Error("str helper")
	}
}
