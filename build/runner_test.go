package build

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/buildscript"
	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
)

var errBoom = errors.New("boom")

// origLogf snapshots the real logf seam so tests can mute it and restore after.
var origLogf = logf

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
		ResolveVersion: func(any, string) (string, string, error) { return "1.2.3", "v1.2.3", nil },
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
	if !strings.Contains(s, `"+openssl.org^1.1"`) || !strings.Contains(s, `"+gnu.org/autoconf"`) || !strings.Contains(s, "make install") {
		t.Errorf("script missing pieces:\n%s", s)
	}
	// no dist → no bottle
	r2 := okRunner("acme.org/tool", tgt)
	res2, err := r2.Build(okRecipe(), "acme.org/tool", "*", tgt, tgt, "")
	if err != nil || res2.BottlePath != "" {
		t.Errorf("no-dist: %+v %v", res2, err)
	}
}

// TestBuildVersionTagExpands proves end-to-end that a distributable URL using
// {{version.tag}} (and the space-padded {{ version.tag }}) is fetched with the
// raw git tag from ResolveVersion — the exact bug that 404'd ~89 recipes — and
// that an empty tag falls back to the version string.
func TestBuildVersionTagExpands(t *testing.T) {
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	distURL := "https://gh/o/r/releases/download/{{version.tag}}/{{ version.tag }}.tar.gz"

	run := func(tag, wantURL string) {
		tenv(t)
		r := okRunner("acme.org/tool", tgt)
		r.ResolveVersion = func(any, string) (string, string, error) { return "1.2.3", tag, nil }
		var fetched string
		r.Fetch = func(url, dest string, _ int) error {
			fetched = url
			return os.MkdirAll(dest, 0o755)
		}
		rec := okRecipe()
		rec.Distributable = distURL
		if _, err := r.Build(rec, "acme.org/tool", "*", tgt, tgt, ""); err != nil {
			t.Fatalf("build: %v", err)
		}
		if fetched != wantURL {
			t.Fatalf("fetched %q; want %q", fetched, wantURL)
		}
	}
	// raw git tag v1.2.3 → both moustache spellings expand to it
	run("v1.2.3", "https://gh/o/r/releases/download/v1.2.3/v1.2.3.tar.gz")
	// empty tag → fall back to the version string
	run("", "https://gh/o/r/releases/download/1.2.3/1.2.3.tar.gz")
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
		writeLibexec = buildscript.WriteLibexec
	}

	cases := map[string]func(r *Runner){
		"resolveversion": func(r *Runner) {
			r.ResolveVersion = func(any, string) (string, string, error) { return "", "", errBoom }
		},
		"removeall": func(r *Runner) { osRemoveAll = func(string) error { return errBoom } },
		"mkdirall":  func(r *Runner) { osMkdirAll = func(string, os.FileMode) error { return errBoom } },
		"mkdir-home": func(r *Runner) {
			n := 0
			osMkdirAll = func(p string, m os.FileMode) error {
				if n++; n >= 2 {
					return errBoom
				}
				return os.MkdirAll(p, m)
			}
		},
		"fetch":        func(r *Runner) { r.Fetch = func(string, string, int) error { return errBoom } },
		"touch":        func(r *Runner) { r.Touch = func(string) error { return errBoom } },
		"writefile":    func(r *Runner) { osWriteFile = func(string, []byte, os.FileMode) error { return errBoom } },
		"writelibexec": func(r *Runner) { writeLibexec = func(string) error { return errBoom } },
		"run":          func(r *Runner) { r.Run = func(string, []string) error { return errBoom } },
		"rename":       func(r *Runner) { osRename = func(string, string) error { return errBoom } },
		"fixup":        func(r *Runner) { r.FixUp = func(fixup.Options) error { return errBoom } },
		"writebottle":  func(r *Runner) { r.WriteBottle = func(_, _, _, _, _, _ string) (string, error) { return "", errBoom } },
		"resolvedep":   func(r *Runner) { r.ResolveDep = func(string, string) (string, error) { return "", errBoom } },
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

func TestSourcesOf(t *testing.T) {
	// a scalar string yields a single candidate
	got, err := sourcesOf("https://x/v{{version.raw}}.tgz", "1.2.3", "v1.2.3")
	if err != nil || len(got) != 1 || got[0].url != "https://x/v1.2.3.tgz" || got[0].git {
		t.Errorf("string dist = %+v %v", got, err)
	}
	// a map yields a single candidate; string strip-components parses
	got, err = sourcesOf(map[string]any{"url": "u-{{version.major}}", "strip-components": "2"}, "3.4.5", "v3.4.5")
	if err != nil || len(got) != 1 || got[0].url != "u-3" || got[0].strip != 2 {
		t.Errorf("map dist = %+v %v", got, err)
	}
	// git via ref + int strip
	got, _ = sourcesOf(map[string]any{"url": "g", "ref": "r{{version.minor}}", "strip-components": 1}, "3.4.5", "v3.4.5")
	if len(got) != 1 || !got[0].git || got[0].ref != "r4" || got[0].strip != 1 {
		t.Errorf("git dist = %+v", got)
	}
	// missing url + unsupported type
	if _, err := sourcesOf(map[string]any{}, "1", "1"); err == nil {
		t.Error("expected missing-url error")
	}
	if _, err := sourcesOf(42, "1", "1"); err == nil {
		t.Error("expected unsupported-type error")
	}
	// list form: EVERY entry is returned, in order — the canonical source first,
	// then fallback mirrors, each keeping its own url + strip-components.
	got, err = sourcesOf([]any{
		map[string]any{"url": "https://up/v{{version.raw}}.tgz", "strip-components": 1},
		map[string]any{"url": "https://mirror/v{{version.raw}}.tgz", "strip-components": 2},
	}, "1.2.3", "v1.2.3")
	if err != nil || len(got) != 2 {
		t.Fatalf("list dist = %+v %v", got, err)
	}
	if got[0].url != "https://up/v1.2.3.tgz" || got[0].strip != 1 {
		t.Errorf("list primary = %+v", got[0])
	}
	if got[1].url != "https://mirror/v1.2.3.tgz" || got[1].strip != 2 {
		t.Errorf("list mirror = %+v", got[1])
	}
	// a bare string entry in a list is equally valid
	if got, _ = sourcesOf([]any{"https://x/v{{version.raw}}.tgz"}, "1.2.3", "v1.2.3"); len(got) != 1 || got[0].url != "https://x/v1.2.3.tgz" {
		t.Errorf("list-of-string dist = %+v", got)
	}
	// an empty list has no candidates → error
	if _, err := sourcesOf([]any{}, "1", "1"); err == nil {
		t.Error("expected empty-list error")
	}
	// a bad entry inside a list surfaces its error
	if _, err := sourcesOf([]any{map[string]any{}}, "1", "1"); err == nil {
		t.Error("expected bad-entry error inside list")
	}
}

// TestSourcesOfVersionTag is the regression proof for the {{version.tag}} bug:
// a GitHub release download URL that interpolates the raw git tag must expand
// to the tag (both the tight and the space-padded moustache form), NOT keep a
// literal {{version.tag}} that 404s at fetch. It also covers the empty-tag
// fallback to the version string.
func TestSourcesOfVersionTag(t *testing.T) {
	// v-prefixed tag, both {{version.tag}} and {{ version.tag }} spellings.
	got, err := sourcesOf("https://gh/o/r/releases/download/{{version.tag}}/{{ version.tag }}.tar.gz", "1.2.3", "v1.2.3")
	if err != nil || len(got) != 1 || got[0].url != "https://gh/o/r/releases/download/v1.2.3/v1.2.3.tar.gz" {
		t.Fatalf("v-tag dist = %+v %v", got, err)
	}
	// a non-"v" upstream tag (e.g. openssl-3.5.0) is preserved verbatim.
	got, err = sourcesOf("https://gh/openssl/openssl/releases/download/{{version.tag}}/openssl-{{version.raw}}.tar.gz", "3.5.0", "openssl-3.5.0")
	if err != nil || len(got) != 1 || got[0].url != "https://gh/openssl/openssl/releases/download/openssl-3.5.0/openssl-3.5.0.tar.gz" {
		t.Fatalf("non-v-tag dist = %+v %v", got, err)
	}
	// empty tag falls back to the version string so the token still expands.
	got, err = sourcesOf("https://x/{{version.tag}}.tgz", "4.5.6", "")
	if err != nil || len(got) != 1 || got[0].url != "https://x/4.5.6.tgz" {
		t.Fatalf("empty-tag fallback dist = %+v %v", got, err)
	}
}

// TestBuildFetchMirrorFallback proves a list-form distributable retries the
// next mirror when an earlier candidate fails, and only fails when all do.
func TestBuildFetchMirrorFallback(t *testing.T) {
	defer func() { logf = origLogf }()
	logf = func(string, ...any) {} // mute the fallback notice
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	project := "acme.org/tool"
	dist := []any{
		map[string]any{"url": "https://primary/v{{version.raw}}.tgz", "strip-components": 1},
		map[string]any{"url": "https://mirror/v{{version.raw}}.tgz", "strip-components": 1},
	}

	// first candidate fails, second succeeds → build proceeds
	t.Run("second-succeeds", func(t *testing.T) {
		tenv(t)
		r := okRunner(project, tgt)
		rec := okRecipe()
		rec.Distributable = dist
		var tried []string
		r.Fetch = func(url, _ string, _ int) error {
			tried = append(tried, url)
			if strings.Contains(url, "primary") {
				return errBoom
			}
			return nil
		}
		if _, err := r.Build(rec, project, "*", tgt, tgt, ""); err != nil {
			t.Fatalf("fallback build should succeed: %v", err)
		}
		if len(tried) != 2 || !strings.Contains(tried[0], "primary") || !strings.Contains(tried[1], "mirror") {
			t.Errorf("tried order = %v", tried)
		}
	})

	// every candidate fails → the build fails with a fetch error
	t.Run("all-fail", func(t *testing.T) {
		tenv(t)
		r := okRunner(project, tgt)
		rec := okRecipe()
		rec.Distributable = dist
		r.Fetch = func(string, string, int) error { return errBoom }
		if _, err := r.Build(rec, project, "*", tgt, tgt, ""); err == nil {
			t.Error("expected fetch error when all mirrors fail")
		}
	})
}

// TestBuildStageInstallDestExists drives the dest-exists rename branch with real
// os calls: Run leaves both the +brewing tree AND the final prefix in place, so
// stage-install must remove the stale prefix and then rename successfully.
func TestBuildStageInstallDestExists(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	project := "acme.org/tool"
	r := okRunner(project, tgt)
	r.Run = func(string, []string) error {
		p := config.Compute(project, "1.2.3", tgt)
		if err := os.MkdirAll(p.BuildInstall, 0o755); err != nil {
			return err
		}
		// a prior/duplicate install already occupies the final versioned prefix
		return os.MkdirAll(p.Install, 0o755)
	}
	if _, err := r.Build(okRecipe(), project, "*", tgt, tgt, ""); err != nil {
		t.Fatalf("dest-exists rename should succeed: %v", err)
	}
	if _, err := os.Stat(config.Compute(project, "1.2.3", tgt).Install); err != nil {
		t.Errorf("final prefix missing after stage: %v", err)
	}
}

// TestBuildStageInstallRemoveError covers the dest-exists RemoveAll failure: the
// final prefix reports as present but cannot be removed, so stage-install errors.
func TestBuildStageInstallRemoveError(t *testing.T) {
	tenv(t)
	defer func() { osStat, osRemoveAll = os.Stat, os.RemoveAll }()
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	project := "acme.org/tool"
	r := okRunner(project, tgt)
	install := config.Compute(project, "1.2.3", tgt).Install
	// Only the stage-install removal (after the build runs) must fail; the initial
	// cleanup loop's removals of the same prefix must still succeed.
	ran := false
	realRun := r.Run
	r.Run = func(p string, e []string) error { ran = true; return realRun(p, e) }
	osStat = func(name string) (os.FileInfo, error) {
		if name == install {
			return nil, nil // report the final prefix as present at stage time
		}
		return os.Stat(name)
	}
	osRemoveAll = func(name string) error {
		if name == install && ran {
			return errBoom // ...but at stage time it cannot be removed
		}
		return os.RemoveAll(name)
	}
	if _, err := r.Build(okRecipe(), project, "*", tgt, tgt, ""); err == nil {
		t.Error("expected stage-install remove error")
	}
}

// restorePropSeams resets the props-copy os seams after a test mutates them.
func restorePropSeams() {
	osStat, osReadDir, osReadFile = os.Stat, os.ReadDir, os.ReadFile
	osMkdirAll, osWriteFile = os.MkdirAll, os.WriteFile
	copyProps = copyPropsTree
}

func TestBuildCopiesProps(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	project := "acme.org/tool"

	// the recipe dir IS props/: a 0644 patch at its root and an executable helper
	// in a sub-directory, exercising file + nested-dir + mode-preservation paths.
	recipeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(recipeDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(recipeDir, "patch.txt")
	if err := os.WriteFile(patch, []byte("the patch"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(patch, 0o644)
	script := filepath.Join(recipeDir, "sub", "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(script, 0o755)

	r := okRunner(project, tgt)
	r.RecipeDir = recipeDir
	rec := okRecipe()
	// a script that references both a relative props path and the {{props}} token
	rec.Build = map[string]any{"script": []any{"patch -p1 < props/patch.txt", "test -f {{props}}/patch.txt"}}
	res, err := r.Build(rec, project, "*", tgt, tgt, "")
	if err != nil {
		t.Fatal(err)
	}

	build := config.Compute(project, "1.2.3", tgt).Build
	// the patch landed at <build>/props/patch.txt with matching contents + mode
	dstPatch := filepath.Join(build, "props", "patch.txt")
	b, err := os.ReadFile(dstPatch)
	if err != nil || string(b) != "the patch" {
		t.Fatalf("props file = %q %v", b, err)
	}
	if fi, _ := os.Stat(dstPatch); fi.Mode().Perm() != 0o644 {
		t.Errorf("patch mode = %v, want 0644", fi.Mode().Perm())
	}
	// the executable helper preserved its 0755 mode through the copy
	if fi, _ := os.Stat(filepath.Join(build, "props", "sub", "run.sh")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("helper mode = %v, want 0755", fi.Mode().Perm())
	}
	// the {{props}} token resolved to the absolute build props dir in the script
	sc, _ := os.ReadFile(res.ScriptPath)
	if want := filepath.Join(build, "props"); !strings.Contains(string(sc), want+"/patch.txt") {
		t.Errorf("script missing resolved {{props}} (%s):\n%s", want, sc)
	}
}

func TestBuildNoProps(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	project := "acme.org/tool"
	// No props copied when RecipeDir is unset (the != "" guard) or points at a
	// path that does not exist (the osStat guard). Both build fine, no props/.
	for _, rd := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		r := okRunner(project, tgt)
		r.RecipeDir = rd
		if _, err := r.Build(okRecipe(), project, "*", tgt, tgt, ""); err != nil {
			t.Fatalf("RecipeDir=%q: %v", rd, err)
		}
		if _, err := os.Stat(filepath.Join(config.Compute(project, "1.2.3", tgt).Build, "props")); !os.IsNotExist(err) {
			t.Errorf("RecipeDir=%q: props dir should not exist: %v", rd, err)
		}
	}
}

func TestBuildPropsCopyError(t *testing.T) {
	tenv(t)
	defer restorePropSeams()
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	recipeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(recipeDir, "props"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyProps = func(string, string) error { return errBoom }
	r := okRunner("acme.org/tool", tgt)
	r.RecipeDir = recipeDir
	if _, err := r.Build(okRecipe(), "acme.org/tool", "*", tgt, tgt, ""); err == nil {
		t.Error("expected copy-props error")
	}
}

func TestCopyPropsTree(t *testing.T) {
	defer restorePropSeams()

	// happy path is covered by TestBuildCopiesProps; here we drive every internal
	// error branch by making one os seam fail at a time.
	mksrc := func(t *testing.T) string {
		d := t.TempDir()
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sub", "b.txt"), []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}

	t.Run("stat-src", func(t *testing.T) {
		defer restorePropSeams()
		osStat = func(string) (os.FileInfo, error) { return nil, errBoom }
		if err := copyPropsTree("s", "d"); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("mkdir-root", func(t *testing.T) {
		defer restorePropSeams()
		osMkdirAll = func(string, os.FileMode) error { return errBoom }
		if err := copyPropsTree(mksrc(t), filepath.Join(t.TempDir(), "out")); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("readdir", func(t *testing.T) {
		defer restorePropSeams()
		osReadDir = func(string) ([]os.DirEntry, error) { return nil, errBoom }
		if err := copyPropsTree(mksrc(t), filepath.Join(t.TempDir(), "out")); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("recurse", func(t *testing.T) {
		defer restorePropSeams()
		n := 0
		osMkdirAll = func(p string, m os.FileMode) error {
			if n++; n >= 2 { // fail the nested sub/ dir, not the root
				return errBoom
			}
			return os.MkdirAll(p, m)
		}
		if err := copyPropsTree(mksrc(t), filepath.Join(t.TempDir(), "out")); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("stat-file", func(t *testing.T) {
		defer restorePropSeams()
		n := 0
		osStat = func(p string) (os.FileInfo, error) {
			if n++; n >= 2 { // src ok, first file stat fails
				return nil, errBoom
			}
			return os.Stat(p)
		}
		if err := copyPropsTree(mksrc(t), filepath.Join(t.TempDir(), "out")); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("readfile", func(t *testing.T) {
		defer restorePropSeams()
		osReadFile = func(string) ([]byte, error) { return nil, errBoom }
		if err := copyPropsTree(mksrc(t), filepath.Join(t.TempDir(), "out")); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("writefile", func(t *testing.T) {
		defer restorePropSeams()
		osWriteFile = func(string, []byte, os.FileMode) error { return errBoom }
		if err := copyPropsTree(mksrc(t), filepath.Join(t.TempDir(), "out")); err != errBoom {
			t.Errorf("err = %v", err)
		}
	})
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
