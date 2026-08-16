package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-attest/sign"
	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/overrides"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bk/versions"
	"github.com/go-pkgx/bottle"
)

// published records what the stubbed publish seam was asked to push.
type publishedBottle struct {
	project, version, tag, osn, arch, dist, path, glibc string
	signed                                              bool
	when                                                time.Time
}

// factoryHarness stubs every factory seam (network, pantry, compiler) and
// collects what the run did.
type factoryHarness struct {
	pantry    string
	failures  string
	detail    string
	published []publishedBottle
	checked   []string // "project@tag" the publish-check was asked about
	built     []string // "project@constraint"
	out, errb bytes.Buffer
}

// newFactoryHarness wires safe defaults: nothing is published yet, every build
// and publish succeeds, requested projects have two versions, deps resolve to
// 9.9.
func newFactoryHarness(t *testing.T) *factoryHarness {
	t.Helper()
	h := &factoryHarness{
		pantry:   t.TempDir(),
		failures: filepath.Join(t.TempDir(), "failures.txt"),
		detail:   filepath.Join(t.TempDir(), "failures-detail.txt"),
	}
	ov, li, re, pu, bu, hp, bf, lp := factoryOverrides, factoryList, factoryResolve, factoryPublish, factoryBuild, factoryHasPlatform, buildFactory, lookPath
	t.Cleanup(func() {
		factoryOverrides, factoryList, factoryResolve, factoryPublish = ov, li, re, pu
		factoryBuild, factoryHasPlatform, buildFactory, lookPath = bu, hp, bf, lp
	})
	for _, k := range []string{"BREWKIT_TARGET", "PLATFORM", "PANTRY", "RECIPES", "DIST", "MAX_VERSIONS", "FORCE", "SIGNING_KEY", "SOURCE_DATE_EPOCH"} {
		t.Setenv(k, "")
	}

	factoryHasPlatform = func(_, project, tag, _, _ string) (bool, error) {
		h.checked = append(h.checked, project+"@"+tag)
		return false, nil
	}
	factoryList = func(any) ([]versions.VersionTag, error) {
		return []versions.VersionTag{{Version: "2.0", Tag: "v2.0"}, {Version: "1.0", Tag: "v1.0"}}, nil
	}
	factoryResolve = func(any, string) (string, string, error) { return "9.9", "v9.9", nil }
	factoryBuild = func(_ *build.Runner, _ *pantry.Recipe, project, constraint string, _, _ target.Target, out string) (build.Result, error) {
		h.built = append(h.built, project+"@"+constraint)
		v := strings.TrimPrefix(constraint, "=")
		return build.Result{Version: v, BottlePath: filepath.Join(out, project, "v"+v+".tar.gz")}, nil
	}
	factoryPublish = func(o publishOptions) (string, error) {
		tag := flavoredTag(o.Project, o.Version, o.Glibc)
		h.published = append(h.published, publishedBottle{
			project: o.Project, version: o.Version, tag: tag, osn: o.OS, arch: o.Arch,
			dist: o.Dist, path: o.Path, glibc: o.Glibc, signed: o.Key != nil, when: o.Time,
		})
		return tag, nil
	}
	buildFactory = func(string) *build.Runner { return &build.Runner{} }
	lookPath = func(s string) (string, error) { return "/usr/local/bin/" + s, nil }
	return h
}

// args builds a command line with the harness's paths plus extras.
func (h *factoryHarness) args(extra ...string) []string {
	return append([]string{
		"--platform", "linux/x86-64",
		"--pantry", h.pantry,
		"--overrides", "",
		"--failures", h.failures,
		"--failures-detail", h.detail,
	}, extra...)
}

func (h *factoryHarness) run(t *testing.T, extra ...string) int {
	t.Helper()
	h.out.Reset()
	h.errb.Reset()
	return runFactory(h.args(extra...), &h.out, &h.errb)
}

func (h *factoryHarness) failuresFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.failures)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *factoryHarness) detailFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.detail)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunFactoryBuildsClosureDepsFirst is the headline behaviour: the requested
// project is expanded to its closure, dependencies build FIRST and at a single
// resolved version, the requested project builds every version, and each bottle
// is published under the registry the run was pointed at.
func TestRunFactoryBuildsClosureDepsFirst(t *testing.T) {
	h := newFactoryHarness(t)
	// missing.org has no recipe here: it is not ours to build (it resolves from
	// upstream dist at build time), so the closure notes it and moves on.
	writeClosureRecipe(t, h.pantry, "app.org", "dependencies:\n  lib.org: '*'\n  missing.org: '*'\nversions:\n  github: a/app/tags\nbuild: make\n")
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")

	if code := h.run(t, "--recipes", "app.org", "--to", "oci://example.test/pkgs"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	if got := strings.Join(h.built, " "); got != "lib.org@=9.9 app.org@=2.0 app.org@=1.0" {
		t.Fatalf("built = %q", got)
	}
	if len(h.published) != 3 {
		t.Fatalf("published = %+v", h.published)
	}
	for _, p := range h.published {
		if p.dist != "oci://example.test/pkgs" || p.osn != "linux" || p.arch != "x86-64" {
			t.Fatalf("published to the wrong place: %+v", p)
		}
		if p.signed {
			t.Fatalf("unsigned run produced a signed bottle: %+v", p)
		}
		if p.when != time.Unix(defaultEpoch, 0).UTC() {
			t.Fatalf("attestation time = %v, want the pinned epoch", p.when)
		}
	}
	if h.published[0].path != filepath.Join("dist", "lib.org", "v9.9.tar.gz") {
		t.Fatalf("bottle path = %q", h.published[0].path)
	}
	out := h.out.String()
	for _, want := range []string{
		"signing: disabled (no key)",
		"closure: 2 project(s) for linux/x86-64 (from 1 requested)",
		"versions app.org: 2 to consider (linux/x86-64)",
		"✅ OK lib.org 9.9 linux/x86-64",
		"=== summary (linux/x86-64): 3 built, 0 skipped, 0 failed ===",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(h.errb.String(), "closure: skip missing.org") {
		t.Fatalf("stderr = %s", h.errb.String())
	}
	if h.failuresFile(t) != "" || h.detailFile(t) != "" {
		t.Fatalf("failure files not empty: %q / %q", h.failuresFile(t), h.detailFile(t))
	}
}

// TestFactorySeamsAreReal exercises the seams the other tests stub out, so the
// production wiring itself is covered rather than only its stand-ins.
func TestFactorySeamsAreReal(t *testing.T) {
	t.Run("build runs the real Runner.Build", func(t *testing.T) {
		tempEnv(t)
		rec, err := pantry.Parse([]byte(miniRecipe))
		if err != nil {
			t.Fatal(err)
		}
		runner := stubFactory("test.org/x")("pkgx")
		res, err := factoryBuild(runner, rec, "test.org/x", "*", target.Host(), target.Host(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if res.Version != "1.0.0" {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("publish-check talks to a registry", func(t *testing.T) {
		m := newMiniOCI()
		defer m.close()
		// nothing has been pushed to this registry: an absent tag is a
		// not-published signal, not an error.
		published, err := factoryHasPlatform(m.base(), "lib.org", "1.0", "linux", "x86-64")
		if err != nil || published {
			t.Fatalf("published = %v, err = %v", published, err)
		}
		if _, err := factoryHasPlatform("://bad", "lib.org", "1.0", "linux", "x86-64"); err == nil {
			t.Fatal("want an error for an unusable registry base")
		}
	})
}

// TestRunFactorySkipIfPublished: an already-published (project, tag, platform)
// is skipped — and --force rebuilds it anyway.
func TestRunFactorySkipIfPublished(t *testing.T) {
	for _, tc := range []struct {
		name              string
		extra             []string
		wantBuilt         int
		wantSkipped       int
		wantSummaryPhrase string
	}{
		{"skip", nil, 0, 1, "0 built, 1 skipped, 0 failed"},
		{"force", []string{"--force"}, 1, 0, "1 built, 0 skipped, 0 failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFactoryHarness(t)
			writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
			factoryHasPlatform = func(_, project, tag, _, _ string) (bool, error) {
				h.checked = append(h.checked, project+"@"+tag)
				return true, nil
			}
			if code := h.run(t, append([]string{"--recipes", "lib.org", "--max-versions", "1"}, tc.extra...)...); code != 0 {
				t.Fatalf("code = %d", code)
			}
			if len(h.built) != tc.wantBuilt {
				t.Fatalf("built = %v", h.built)
			}
			if !strings.Contains(h.out.String(), tc.wantSummaryPhrase) {
				t.Fatalf("stdout = %s", h.out.String())
			}
			if tc.wantSkipped > 0 && !strings.Contains(h.out.String(), "⏭  SKIP lib.org 2.0 (linux/x86-64) — already published") {
				t.Fatalf("stdout = %s", h.out.String())
			}
			if tc.name == "force" && len(h.checked) != 0 {
				t.Fatalf("--force still queried the registry: %v", h.checked)
			}
		})
	}
}

// TestRunFactoryPublishCheckErrorBuildsAnyway: an unreachable registry must not
// silently skip the world — the check failure is reported and the build runs.
func TestRunFactoryPublishCheckErrorBuildsAnyway(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	factoryHasPlatform = func(string, string, string, string, string) (bool, error) {
		return false, errors.New("dial tcp: no route to host")
	}
	if code := h.run(t, "--recipes", "lib.org", "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if len(h.built) != 1 {
		t.Fatalf("built = %v", h.built)
	}
	if !strings.Contains(h.errb.String(), "publish-check lib.org 2.0: dial tcp") {
		t.Fatalf("stderr = %s", h.errb.String())
	}
}

// TestRunFactoryMaxVersions caps a requested project to the newest N versions.
func TestRunFactoryMaxVersions(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	if code := h.run(t, "--recipes", "lib.org", "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if got := strings.Join(h.built, " "); got != "lib.org@=2.0" {
		t.Fatalf("built = %q, want only the newest", got)
	}
}

// TestRunFactoryGlibcFlavor: --glibc implies the pkgx libc and flavors both the
// published tag and the already-published check.
func TestRunFactoryGlibcFlavor(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	writeClosureRecipe(t, h.pantry, "gnu.org/glibc", "versions:\n  github: a/glibc/tags\nbuild: make\n")
	var runner *build.Runner
	buildFactory = func(string) *build.Runner { runner = &build.Runner{}; return runner }

	if code := h.run(t, "--recipes", "lib.org gnu.org/glibc", "--glibc", "2.27.0", "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	if runner.LibcMode != "pkgx" || runner.Glibc != "2.27.0" {
		t.Fatalf("runner libc = %q glibc = %q", runner.LibcMode, runner.Glibc)
	}
	if got := strings.Join(h.checked, " "); got != "lib.org@2.0-glibc2.27.0 gnu.org/glibc@2.0" {
		t.Fatalf("checked = %q (glibc itself must not be flavored)", got)
	}
	if h.published[0].tag != "2.0-glibc2.27.0" || h.published[1].tag != "2.0" {
		t.Fatalf("tags = %q %q", h.published[0].tag, h.published[1].tag)
	}
	if !strings.Contains(h.out.String(), "✅ OK lib.org 2.0-glibc2.27.0 linux/x86-64") {
		t.Fatalf("stdout = %s", h.out.String())
	}
}

// TestRunFactoryFailureStages: every way one recipe can fail is recorded in
// failures.txt (+ detail) and never fails the run.
func TestRunFactoryFailureStages(t *testing.T) {
	t.Run("build, with the error tail captured", func(t *testing.T) {
		h := newFactoryHarness(t)
		writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
		script := filepath.Join(t.TempDir(), "build.sh")
		if err := os.WriteFile(script, []byte("echo compiling lib\necho 'error: boom' >&2\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		factoryBuild = func(r *build.Runner, _ *pantry.Recipe, _, _ string, _, _ target.Target, _ string) (build.Result, error) {
			if err := r.Run(script, nil); err != nil { // the real script runner, teed into the tail buffer
				t.Fatal(err)
			}
			return build.Result{}, errors.New("stage install: rename: file exists")
		}
		if code := h.run(t, "--recipes", "lib.org", "--max-versions", "1"); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org 2.0 build\n" {
			t.Fatalf("failures.txt = %q", got)
		}
		detail := h.detailFile(t)
		for _, want := range []string{
			"########## lib.org 2.0 (linux/x86-64) — build failed: stage install: rename: file exists",
			"compiling lib", "error: boom",
		} {
			if !strings.Contains(detail, want) {
				t.Fatalf("failures-detail.txt missing %q:\n%s", want, detail)
			}
		}
		// the build's own output still reaches the log, inside a fold
		if !strings.Contains(h.out.String(), "::group::build lib.org 2.0 (linux/x86-64)\ncompiling lib") {
			t.Fatalf("stdout = %s", h.out.String())
		}
		if !strings.Contains(h.out.String(), "1 failed") {
			t.Fatalf("stdout = %s", h.out.String())
		}
	})

	for _, tc := range []struct {
		name, want string
		setup      func(t *testing.T, h *factoryHarness)
	}{
		{"package", "lib.org 2.0 package\n", func(t *testing.T, h *factoryHarness) {
			factoryBuild = func(_ *build.Runner, _ *pantry.Recipe, _, _ string, _, _ target.Target, _ string) (build.Result, error) {
				return build.Result{Version: "2.0"}, nil // built, but nothing bottled
			}
		}},
		{"publish", "lib.org 2.0 publish\n", func(t *testing.T, h *factoryHarness) {
			factoryPublish = func(publishOptions) (string, error) { return "", errors.New("403 denied") }
		}},
		{"versions listing", "lib.org latest versions\n", func(t *testing.T, h *factoryHarness) {
			factoryList = func(any) ([]versions.VersionTag, error) { return nil, errors.New("spec has neither github nor url") }
		}},
		{"versions empty", "lib.org latest versions\n", func(t *testing.T, h *factoryHarness) {
			factoryList = func(any) ([]versions.VersionTag, error) { return nil, nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFactoryHarness(t)
			writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
			tc.setup(t, h)
			if code := h.run(t, "--recipes", "lib.org", "--max-versions", "1"); code != 0 {
				t.Fatalf("code = %d", code)
			}
			if got := h.failuresFile(t); got != tc.want {
				t.Fatalf("failures.txt = %q, want %q", got, tc.want)
			}
			if !strings.Contains(h.out.String(), "0 built, 0 skipped, 1 failed") {
				t.Fatalf("stdout = %s", h.out.String())
			}
			if !strings.Contains(h.out.String(), "failures:\n"+tc.want) {
				t.Fatalf("summary did not list the failure:\n%s", h.out.String())
			}
		})
	}

	t.Run("dependency version resolution", func(t *testing.T) {
		h := newFactoryHarness(t)
		writeClosureRecipe(t, h.pantry, "app.org", "dependencies:\n  lib.org: '*'\nversions:\n  github: a/app/tags\nbuild: make\n")
		writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
		factoryResolve = func(any, string) (string, string, error) {
			return "", "", errors.New("no candidate version matched")
		}
		if code := h.run(t, "--recipes", "app.org", "--max-versions", "1"); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org latest versions\n" {
			t.Fatalf("failures.txt = %q", got)
		}
		if got := strings.Join(h.built, " "); got != "app.org@=2.0" {
			t.Fatalf("a failed dep must not stop the rest: built = %q", got)
		}
	})

	t.Run("unparseable recipe", func(t *testing.T) {
		h := newFactoryHarness(t)
		writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
		// closureOf parsed the recipe already; corrupt it before the build loop.
		closure := closureOf
		t.Cleanup(func() { closureOf = closure })
		closureOf = func(dir string, tgt target.Target, want []string, warn func(string)) []string {
			writeClosureRecipe(t, h.pantry, "lib.org", "versions: [\n")
			return want
		}
		if code := h.run(t, "--recipes", "lib.org"); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org latest recipe\n" {
			t.Fatalf("failures.txt = %q", got)
		}
	})
}

// TestRunFactorySkipsProjectWithoutRecipe guards the loop against a project the
// pantry cannot supply (e.g. removed between the closure walk and the build).
func TestRunFactorySkipsProjectWithoutRecipe(t *testing.T) {
	h := newFactoryHarness(t)
	closure := closureOf
	t.Cleanup(func() { closureOf = closure })
	closureOf = func(string, target.Target, []string, func(string)) []string { return []string{"ghost.org"} }
	if code := h.run(t, "--recipes", "ghost.org"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.out.String(), "SKIP ghost.org (no recipe)") {
		t.Fatalf("stdout = %s", h.out.String())
	}
	if len(h.built) != 0 {
		t.Fatalf("built = %v", h.built)
	}
}

// TestRunFactoryOverridesApplied: the pantry is patched before the closure is
// computed, and an override failure is fatal only when the directory itself is
// unusable.
func TestRunFactoryOverridesApplied(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	var gotDir, gotRoot string
	factoryOverrides = func(o overrides.Options) (overrides.Result, error) {
		gotDir, gotRoot = o.Dir, o.Root
		o.Log("override applied: x.patch")
		o.Warn("override SKIP (does not apply): y.patch")
		return overrides.Result{Applied: []string{"x.patch"}}, nil
	}
	if code := h.run(t, "--recipes", "lib.org", "--overrides", "over", "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if gotDir != "over" || gotRoot != h.pantry {
		t.Fatalf("overrides applied to %q / %q", gotDir, gotRoot)
	}
	if !strings.Contains(h.out.String(), "override applied: x.patch") || !strings.Contains(h.errb.String(), "y.patch") {
		t.Fatalf("out = %s err = %s", h.out.String(), h.errb.String())
	}

	factoryOverrides = func(overrides.Options) (overrides.Result, error) {
		return overrides.Result{}, errors.New("overrides: syntax error in pattern")
	}
	if code := h.run(t, "--recipes", "lib.org", "--overrides", "over"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

// TestRunFactorySigning: a key from a file or straight from $SIGNING_KEY (as CI
// provides it) reaches the publisher; an unreadable key is fatal.
func TestRunFactorySigning(t *testing.T) {
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyText := kp.SecretKeyFile("test")

	t.Run("key file", func(t *testing.T) {
		h := newFactoryHarness(t)
		writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte(keyText), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := h.run(t, "--recipes", "lib.org", "--sign", path, "--max-versions", "1"); code != 0 {
			t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
		}
		if !h.published[0].signed || !strings.Contains(h.out.String(), "signing: enabled") {
			t.Fatalf("bottle not signed: %+v / %s", h.published, h.out.String())
		}
	})

	t.Run("SIGNING_KEY env", func(t *testing.T) {
		h := newFactoryHarness(t)
		writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
		t.Setenv("SIGNING_KEY", keyText)
		if code := h.run(t, "--recipes", "lib.org", "--max-versions", "1"); code != 0 {
			t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
		}
		if !h.published[0].signed {
			t.Fatalf("bottle not signed: %+v", h.published)
		}
	})

	t.Run("unreadable key", func(t *testing.T) {
		h := newFactoryHarness(t)
		if code := h.run(t, "--recipes", "lib.org", "--sign", filepath.Join(t.TempDir(), "absent")); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(h.errb.String(), "no such file") {
			t.Fatalf("stderr = %s", h.errb.String())
		}
	})
}

// TestRunFactoryRecipesSources: the want list comes from --recipes, else from
// the recipes file (comments and blanks dropped).
func TestRunFactoryRecipesSources(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	list := filepath.Join(t.TempDir(), "recipes.txt")
	if err := os.WriteFile(list, []byte("# a comment\n\nlib.org\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "--recipes-file", list, "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	if got := strings.Join(h.built, " "); got != "lib.org@=2.0" {
		t.Fatalf("built = %q", got)
	}

	if code := h.run(t, "--recipes-file", filepath.Join(t.TempDir(), "absent.txt")); code != 1 {
		t.Fatalf("missing recipes file: code = %d, want 1", code)
	}
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, []byte("# nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "--recipes-file", empty); code != 1 {
		t.Fatalf("empty recipes file: code = %d, want 1", code)
	}
	if !strings.Contains(h.errb.String(), "nothing to build") {
		t.Fatalf("stderr = %s", h.errb.String())
	}
}

// TestRunFactoryUsageErrors covers the argument/target validation.
func TestRunFactoryUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"bad flag", []string{"--nope"}, 2},
		{"no platform", []string{"--recipes", "lib.org"}, 2},
		{"bad platform", []string{"--platform", "linux", "--recipes", "lib.org"}, 2},
		{"unsupported arch", []string{"--platform", "linux/frobnicate", "--recipes", "lib.org"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFactoryHarness(t)
			var out, errb bytes.Buffer
			if code := runFactory(tc.args, &out, &errb); code != tc.want {
				t.Fatalf("code = %d, want %d (stderr %s)", code, tc.want, errb.String())
			}
			_ = h
		})
	}
}

// TestRunFactoryPkgxLookupFallback: an unresolvable pkgx name is passed through
// verbatim rather than swallowed.
func TestRunFactoryPkgxLookupFallback(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	var gotBin string
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	buildFactory = func(bin string) *build.Runner { gotBin = bin; return &build.Runner{} }
	if code := h.run(t, "--recipes", "lib.org", "--pkgx", "/opt/pkgx", "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if gotBin != "/opt/pkgx" {
		t.Fatalf("pkgx bin = %q", gotBin)
	}
}

// TestRunFactoryFailureFileWriteErrors: an unwritable report path is reported,
// not fatal — the bottles are already published.
func TestRunFactoryFailureFileWriteErrors(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	h.failures = filepath.Join(t.TempDir(), "absent-dir", "failures.txt")
	h.detail = filepath.Join(t.TempDir(), "absent-dir", "detail.txt")
	if code := h.run(t, "--recipes", "lib.org", "--max-versions", "1"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if n := strings.Count(h.errb.String(), "no such file or directory"); n != 2 {
		t.Fatalf("stderr = %s", h.errb.String())
	}
	if !strings.Contains(h.out.String(), "1 built") {
		t.Fatalf("stdout = %s", h.out.String())
	}
}

// withMirrorSeams stubs the upstream listing + download and restores the
// bottle package's dist pointer.
func withMirrorSeams(t *testing.T, versions map[string][]string, download func(project, ver, osn, arch string) ([]byte, string, error)) {
	t.Helper()
	uv, dl, dist := factoryUpstreamVersions, factoryDownload, bottle.DistBase
	t.Cleanup(func() { factoryUpstreamVersions, factoryDownload, bottle.DistBase = uv, dl, dist })
	factoryUpstreamVersions = func(project, _, _ string) ([]bottle.Ver, error) {
		vs, ok := versions[project]
		if !ok {
			return nil, errors.New("404 not found")
		}
		var out []bottle.Ver // upstream lists ascending
		for _, v := range vs {
			out = append(out, bottle.ParseVer(v))
		}
		return out, nil
	}
	factoryDownload = download
}

// TestRunFactoryMirrors is the sweep path: copy upstream bottles into our
// registry, re-signed and attested, without building anything.
func TestRunFactoryMirrors(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "gnu.org/glibc", "versions:\n  github: a/glibc/tags\nbuild: make\n")
	withMirrorSeams(t, map[string][]string{"gnu.org/glibc": {"2.27.0", "2.38.0", "2.44.0"}},
		func(_, ver, _, _ string) ([]byte, string, error) { return []byte("bottle-" + ver), ".tar.xz", nil })

	if code := h.run(t, "--recipes", "gnu.org/glibc", "--mirror-from", "https://dist.pkgx.dev/",
		"--bottles", filepath.Join(t.TempDir(), "dist")); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	if len(h.built) != 0 {
		t.Fatalf("mirror mode must not build: %v", h.built)
	}
	if bottle.DistBase != "https://dist.pkgx.dev" {
		t.Fatalf("upstream dist = %q (trailing slash must be trimmed)", bottle.DistBase)
	}
	if os.Getenv("PKGX_VERIFY") != "0" {
		t.Fatalf("PKGX_VERIFY = %q; an unsigned upstream cannot be copied fail-closed", os.Getenv("PKGX_VERIFY"))
	}
	// newest first, every version, and the staged tarball is what got published
	var tags []string
	for _, p := range h.published {
		tags = append(tags, p.tag)
		if filepath.Base(p.path) != "v"+p.version+".tar.xz" {
			t.Fatalf("staged path = %q", p.path)
		}
		b, err := os.ReadFile(p.path)
		if err != nil || string(b) != "bottle-"+p.version {
			t.Fatalf("staged bytes = %q, err = %v", b, err)
		}
	}
	if strings.Join(tags, ",") != "2.44.0,2.38.0,2.27.0" {
		t.Fatalf("tags = %v", tags)
	}

	// --max-versions caps the copy at the newest N, as it does for builds
	h2 := newFactoryHarness(t)
	writeClosureRecipe(t, h2.pantry, "gnu.org/glibc", "versions:\n  github: a/glibc/tags\nbuild: make\n")
	withMirrorSeams(t, map[string][]string{"gnu.org/glibc": {"2.27.0", "2.38.0", "2.44.0"}},
		func(_, ver, _, _ string) ([]byte, string, error) { return []byte(ver), ".tar.gz", nil })
	if code := h2.run(t, "--recipes", "gnu.org/glibc", "--mirror-from", "https://d",
		"--max-versions", "2", "--bottles", filepath.Join(t.TempDir(), "dist")); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if len(h2.published) != 2 || h2.published[0].version != "2.44.0" || h2.published[1].version != "2.38.0" {
		t.Fatalf("capped copy = %+v", h2.published)
	}
	out := h.out.String()
	for _, want := range []string{
		"mirror: copying bottles from https://dist.pkgx.dev (no build)",
		"versions gnu.org/glibc: 3 to consider (linux/x86-64, upstream)",
		"✅ MIRRORED gnu.org/glibc 2.44.0 linux/x86-64",
		"=== summary (linux/x86-64): 3 built, 0 skipped, 0 failed ===",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestRunFactoryMirrorNeedsNoRecipe: mirroring is recipe-free — a project the
// closure walk drops for want of a recipe is still copied, and a dependency
// only gets its newest.
func TestRunFactoryMirrorNeedsNoRecipe(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "app.org", "dependencies:\n  lib.org: '*'\nversions:\n  github: a/app/tags\nbuild: make\n")
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	withMirrorSeams(t, map[string][]string{
		"app.org":      {"1.0.0", "2.0.0"},
		"lib.org":      {"0.1.0", "0.2.0"},
		"norecipe.org": {"9.0.0"},
	}, func(_, ver, _, _ string) ([]byte, string, error) { return []byte(ver), ".tar.gz", nil })

	if code := h.run(t, "--recipes", "app.org norecipe.org", "--mirror-from", "https://dist.example",
		"--bottles", filepath.Join(t.TempDir(), "dist")); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	var got []string
	for _, p := range h.published {
		got = append(got, p.project+"@"+p.version)
	}
	// deps first, one version for the dep, every version for the requested ones
	if strings.Join(got, " ") != "lib.org@0.2.0 app.org@2.0.0 app.org@1.0.0 norecipe.org@9.0.0" {
		t.Fatalf("published = %v", got)
	}
}

// TestRunFactoryMirrorFailures: nothing upstream, a fetch error, an unwritable
// staging dir and a publish error are each recorded, never fatal.
func TestRunFactoryMirrorFailures(t *testing.T) {
	stage := func(t *testing.T) string { return filepath.Join(t.TempDir(), "dist") }

	t.Run("no upstream bottle", func(t *testing.T) {
		h := newFactoryHarness(t)
		withMirrorSeams(t, map[string][]string{"lib.org": nil}, nil)
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", stage(t)); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org latest versions\n" {
			t.Fatalf("failures = %q", got)
		}
		if !strings.Contains(h.detailFile(t), "no upstream bottle for linux/x86-64") {
			t.Fatalf("detail = %q", h.detailFile(t))
		}
	})

	t.Run("upstream listing fails", func(t *testing.T) {
		h := newFactoryHarness(t)
		withMirrorSeams(t, nil, nil)
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", stage(t)); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org latest versions\n" {
			t.Fatalf("failures = %q", got)
		}
	})

	t.Run("fetch fails", func(t *testing.T) {
		h := newFactoryHarness(t)
		withMirrorSeams(t, map[string][]string{"lib.org": {"1.0.0"}},
			func(string, string, string, string) ([]byte, string, error) {
				return nil, "", errors.New("502 bad gateway")
			})
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", stage(t)); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org 1.0.0 fetch\n" {
			t.Fatalf("failures = %q", got)
		}
	})

	t.Run("staging dir unwritable", func(t *testing.T) {
		h := newFactoryHarness(t)
		withMirrorSeams(t, map[string][]string{"lib.org": {"1.0.0"}},
			func(string, string, string, string) ([]byte, string, error) { return []byte("x"), ".tar.gz", nil })
		blocked := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocked, nil, 0o644); err != nil { // a FILE where the tree must go
			t.Fatal(err)
		}
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", blocked); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org 1.0.0 fetch\n" {
			t.Fatalf("failures = %q", got)
		}
	})

	t.Run("write fails", func(t *testing.T) {
		h := newFactoryHarness(t)
		withMirrorSeams(t, map[string][]string{"lib.org": {"1.0.0"}},
			func(string, string, string, string) ([]byte, string, error) { return []byte("x"), ".tar.gz", nil })
		// a leftover DIRECTORY where the tarball must be written
		bottles := stage(t)
		if err := os.MkdirAll(filepath.Join(bottles, "lib.org", "linux", "x86-64", "v1.0.0.tar.gz"), 0o755); err != nil {
			t.Fatal(err)
		}
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", bottles); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org 1.0.0 fetch\n" {
			t.Fatalf("failures = %q", got)
		}
	})

	t.Run("publish fails", func(t *testing.T) {
		h := newFactoryHarness(t)
		withMirrorSeams(t, map[string][]string{"lib.org": {"1.0.0"}},
			func(string, string, string, string) ([]byte, string, error) { return []byte("x"), ".tar.gz", nil })
		factoryPublish = func(publishOptions) (string, error) { return "", errors.New("403 denied") }
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", stage(t)); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if got := h.failuresFile(t); got != "lib.org 1.0.0 publish\n" {
			t.Fatalf("failures = %q", got)
		}
	})
}

// TestRunFactoryMirrorSkipAndForce: an already-published version is skipped, an
// unreachable registry still mirrors, and --force re-copies.
func TestRunFactoryMirrorSkipAndForce(t *testing.T) {
	newHarness := func(t *testing.T) *factoryHarness {
		h := newFactoryHarness(t)
		withMirrorSeams(t, map[string][]string{"lib.org": {"1.0.0"}},
			func(string, string, string, string) ([]byte, string, error) { return []byte("x"), ".tar.gz", nil })
		return h
	}
	t.Run("skip", func(t *testing.T) {
		h := newHarness(t)
		factoryHasPlatform = func(string, string, string, string, string) (bool, error) { return true, nil }
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", filepath.Join(t.TempDir(), "d")); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if len(h.published) != 0 || !strings.Contains(h.out.String(), "0 built, 1 skipped, 0 failed") {
			t.Fatalf("published = %v\n%s", h.published, h.out.String())
		}
	})
	t.Run("force", func(t *testing.T) {
		h := newHarness(t)
		factoryHasPlatform = func(string, string, string, string, string) (bool, error) { return true, nil }
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--force", "--bottles", filepath.Join(t.TempDir(), "d")); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if len(h.published) != 1 {
			t.Fatalf("published = %v", h.published)
		}
	})
	t.Run("registry unreachable", func(t *testing.T) {
		h := newHarness(t)
		factoryHasPlatform = func(string, string, string, string, string) (bool, error) {
			return false, errors.New("dial tcp: refused")
		}
		if code := h.run(t, "--recipes", "lib.org", "--mirror-from", "https://d", "--bottles", filepath.Join(t.TempDir(), "d")); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if len(h.published) != 1 || !strings.Contains(h.errb.String(), "publish-check lib.org 1.0.0") {
			t.Fatalf("published = %v, stderr = %s", h.published, h.errb.String())
		}
	})
}

// TestSetUpstreamDistKeepsAnExplicitVerify: an operator who demanded
// verification keeps it (the copy then fails closed, as asked).
func TestSetUpstreamDistKeepsAnExplicitVerify(t *testing.T) {
	old := bottle.DistBase
	t.Cleanup(func() { bottle.DistBase = old })
	t.Setenv("PKGX_VERIFY", "1")
	setUpstreamDist("https://dist.example")
	if os.Getenv("PKGX_VERIFY") != "1" {
		t.Fatalf("PKGX_VERIFY = %q", os.Getenv("PKGX_VERIFY"))
	}
}

// TestFactoryUpstreamSeamsAreReal exercises the production wiring of the
// mirror seams (they are stubbed everywhere else).
func TestFactoryUpstreamSeamsAreReal(t *testing.T) {
	old := bottle.DistBase
	t.Cleanup(func() { bottle.DistBase = old })
	t.Setenv("PKGX_VERIFY", "0")
	bottle.DistBase = "https://127.0.0.1:1/dist" // nothing listening
	if _, err := factoryUpstreamVersions("lib.org", "linux", "x86-64"); err == nil {
		t.Fatal("want a listing error")
	}
	if _, _, err := factoryDownload("lib.org", "1.0.0", "linux", "x86-64"); err == nil {
		t.Fatal("want a download error")
	}
}

// TestFactoryDispatch covers the main-loop `case "factory"` route.
func TestFactoryDispatch(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	code, out, errs := run2(t, "factory", "--platform", "linux/x86-64", "--pantry", h.pantry,
		"--overrides", "", "--failures", h.failures, "--failures-detail", h.detail,
		"--recipes", "lib.org", "--max-versions", "1")
	if code != 0 {
		t.Fatalf("dispatch factory code=%d err=%q", code, errs)
	}
	if !strings.Contains(out, "1 built, 0 skipped, 0 failed") {
		t.Fatalf("dispatch factory out=%q", out)
	}
}

func TestFactoryTime(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "")
	if got := factoryTime(); got != time.Unix(defaultEpoch, 0).UTC() {
		t.Fatalf("unset epoch → %v, want the pinned default", got)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "1234567890")
	if got := factoryTime(); got != time.Unix(1234567890, 0).UTC() {
		t.Fatalf("epoch honoured? got %v", got)
	}
}

func TestEnvInt(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want int
	}{{"", 0}, {"abc", 0}, {"-3", 0}, {"7", 7}} {
		t.Setenv("BK_TEST_N", tc.val)
		if got := envInt("BK_TEST_N"); got != tc.want {
			t.Fatalf("envInt(%q) = %d, want %d", tc.val, got, tc.want)
		}
	}
}

func TestFactoryWantFields(t *testing.T) {
	got, err := factoryWant("  a.org   b.org ", "/nonexistent")
	if err != nil || strings.Join(got, ",") != "a.org,b.org" {
		t.Fatalf("got %v, err %v", got, err)
	}
}

// TestTailWriter: output passes through untouched while only the last lines are
// remembered, including a trailing partial line.
func TestTailWriter(t *testing.T) {
	var sink bytes.Buffer
	tw := &tailWriter{w: &sink, max: 2}
	for _, s := range []string{"one\ntwo\n", "three\nfour", "teen\n", "five"} {
		n, err := tw.Write([]byte(s))
		if err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if sink.String() != "one\ntwo\nthree\nfourteen\nfive" {
		t.Fatalf("pass-through = %q", sink.String())
	}
	if got := tw.tail(); got != "three\nfourteen\nfive" {
		t.Fatalf("tail = %q", got)
	}
}

// TestTailWriterWriteError propagates the underlying writer's error.
func TestTailWriterWriteError(t *testing.T) {
	tw := &tailWriter{w: errWriter{}, max: 2}
	if _, err := tw.Write([]byte("x\n")); err == nil {
		t.Fatal("want error")
	}
	if tw.tail() != "" {
		t.Fatalf("tail = %q", tw.tail())
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// TestRunFactoryVersionConstraint: --versions selects a LINE, which "newest N"
// cannot. Our registry carries cmake 4.4.2 while 114 pantry recipes pin
// `cmake.org: ^3`; no value of --max-versions reaches a 3.x from a newest-first
// listing, so publishing the version those recipes actually need needs this.
func TestRunFactoryVersionConstraint(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	if code := h.run(t, "--recipes", "lib.org", "--versions", "^1"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	if got := strings.Join(h.built, " "); got != "lib.org@=1.0" {
		t.Fatalf("built = %q, want only the 1.x line", got)
	}
	if !strings.Contains(h.out.String(), `versions lib.org: 1 dropped by --versions "^1"`) {
		t.Fatalf("the drop must be said, not silent:\n%s", h.out.String())
	}
}

// A constraint no version satisfies is a failure, not an empty success.
func TestRunFactoryVersionConstraintMatchesNothing(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "lib.org", "versions:\n  github: a/lib/tags\nbuild: make\n")
	// The factory tolerates per-project failures by design (it records them and
	// keeps going), so what must be true is that NOTHING was built and the
	// failure is on the record -- not that the process died.
	h.run(t, "--recipes", "lib.org", "--versions", "^9")
	if len(h.built) != 0 {
		t.Fatalf("nothing may be built: %v", h.built)
	}
	if !strings.Contains(h.failuresFile(t), "lib.org  versions") && !strings.Contains(h.errb.String()+h.out.String(), `no version matches "^9"`) {
		t.Fatalf("failure not reported:\n%s\n%s", h.out.String(), h.failuresFile(t))
	}
}

// The same handle applies to mirror mode, which is where it is actually needed:
// upstream publishes cmake 3.x bottles our registry lacks.
func TestRunFactoryMirrorVersionConstraint(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "cmake.org", "versions:\n  github: a/cmake/tags\nbuild: make\n")
	withMirrorSeams(t, map[string][]string{"cmake.org": {"3.31.12", "4.4.1", "4.4.2"}},
		func(_, ver, _, _ string) ([]byte, string, error) { return []byte("bottle-" + ver), ".tar.xz", nil })

	if code := h.run(t, "--recipes", "cmake.org", "--mirror-from", "https://dist.pkgx.dev",
		"--versions", "^3", "--bottles", filepath.Join(t.TempDir(), "dist")); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, h.errb.String())
	}
	var tags []string
	for _, p := range h.published {
		tags = append(tags, p.tag)
	}
	if strings.Join(tags, ",") != "3.31.12" {
		t.Fatalf("tags = %v, want only the 3.x line", tags)
	}
}

// …and in mirror mode too, an unsatisfiable constraint is a recorded failure,
// not a silent no-op that looks like "everything was already published".
func TestRunFactoryMirrorVersionConstraintMatchesNothing(t *testing.T) {
	h := newFactoryHarness(t)
	writeClosureRecipe(t, h.pantry, "cmake.org", "versions:\n  github: a/cmake/tags\nbuild: make\n")
	withMirrorSeams(t, map[string][]string{"cmake.org": {"4.4.1", "4.4.2"}},
		func(_, ver, _, _ string) ([]byte, string, error) { return []byte("bottle-" + ver), ".tar.xz", nil })

	h.run(t, "--recipes", "cmake.org", "--mirror-from", "https://dist.pkgx.dev",
		"--versions", "^3", "--bottles", filepath.Join(t.TempDir(), "dist"))
	if len(h.published) != 0 {
		t.Fatalf("nothing may be published: %v", h.published)
	}
	if !strings.Contains(h.failuresFile(t), "cmake.org") {
		t.Fatalf("failure not recorded: %q", h.failuresFile(t))
	}
}
