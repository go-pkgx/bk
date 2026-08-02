package build

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgx/bk/target"
)

func lin() target.Target { return target.Target{Platform: "linux", Arch: "x86-64"} }
func win() target.Target { return target.Target{Platform: "windows", Arch: "x86-64"} }

func TestDepSpecs(t *testing.T) {
	deps := map[string]any{
		"openssl.org":    "^1.1",
		"linux":          map[string]any{"gnu.org/A": "*"},
		"darwin":         map[string]any{"apple.B": "1"},
		"windows/x86-64": map[string]any{"llvm.org/mingw-w64": nil},
		"x86-64":         map[string]any{"arch.only": "2"},
		"aarch64":        map[string]any{"nope": "9"},
	}
	// linux target: openssl + linux sub + x86-64 sub; darwin/windows/aarch64 dropped
	got := DepSpecs(deps, lin())
	want := []string{"arch.only@2", "gnu.org/A", "openssl.org@^1.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linux DepSpecs = %v want %v", got, want)
	}
	// windows/x86-64 target: openssl + windows sub (bare) + x86-64 sub
	got = DepSpecs(deps, win())
	want = []string{"arch.only@2", "llvm.org/mingw-w64", "openssl.org@^1.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("windows DepSpecs = %v want %v", got, want)
	}
	// a platform key whose value isn't a map is ignored
	if g := DepSpecs(map[string]any{"linux": "notamap", "x": "1"}, lin()); !reflect.DeepEqual(g, []string{"x@1"}) {
		t.Errorf("non-map platform = %v", g)
	}
	if len(DepSpecs(nil, lin())) != 0 {
		t.Error("nil deps")
	}
}

func TestValStr(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{{nil, "*"}, {"^1", "^1"}, {true, "*"}, {false, ""}, {5, "5"}, {3.0, "3"}}
	for _, c := range cases {
		if got := valStr(c.v); got != c.want {
			t.Errorf("valStr(%v)=%q want %q", c.v, got, c.want)
		}
	}
	// a bare-star constraint yields a bare project spec
	if g := DepSpecs(map[string]any{"p": "*"}, lin()); !reflect.DeepEqual(g, []string{"p"}) {
		t.Errorf("star = %v", g)
	}
}

func TestPlatformKey(t *testing.T) {
	for k, exp := range map[string][3]string{
		"linux":          {"linux", "", "y"},
		"x86-64":         {"", "x86-64", "y"},
		"windows/x86-64": {"windows", "x86-64", "y"},
		"openssl.org":    {"", "", ""},
	} {
		os, arch, ok := platformKey(k)
		if os != exp[0] || arch != exp[1] || (ok != (exp[2] == "y")) {
			t.Errorf("platformKey(%q)=%q,%q,%v", k, os, arch, ok)
		}
	}
}

func TestBaseToolchainAndEvalDeps(t *testing.T) {
	base := BaseToolchain()
	if len(base) == 0 || !contains(base, "gnu.org/autoconf") || !contains(base, "gnu.org/texinfo") {
		t.Errorf("base toolchain = %v", base)
	}
	// EvalDeps = base + runtime + build, deduped by project, sorted
	got := EvalDeps(
		map[string]any{"openssl.org": "^1.1"},
		map[string]any{"freedesktop.org/pkg-config": "^0.29"}, // already in base → dedup
		lin(),
	)
	if !contains(got, "openssl.org@^1.1") || !contains(got, "gnu.org/autoconf") {
		t.Errorf("EvalDeps missing entries: %v", got)
	}
	// pkg-config appears exactly once (base's bare form wins)
	n := 0
	for _, s := range got {
		if strings.HasPrefix(s, "freedesktop.org/pkg-config") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("pkg-config not deduped: %v", got)
	}
	// sorted
	if !sortedStrings(got) {
		t.Errorf("EvalDeps not sorted: %v", got)
	}
}

func TestSanitizedEnv(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("GITHUB_TOKEN", "secret")
	os.Unsetenv("PKGX_PANTRY_DIR")
	env := SanitizedEnv("/h", "/pkgx")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=/h", "PKGX_DIR=/pkgx", "TERM=xterm", "GITHUB_TOKEN=secret"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SanitizedEnv missing %q in %v", want, env)
		}
	}
	if strings.Contains(joined, "PKGX_PANTRY_DIR") {
		t.Error("unset var leaked into SanitizedEnv")
	}
}

func TestTouchAutotools(t *testing.T) {
	dir := t.TempDir()
	files := []string{"configure.ac", "acinclude.m4", "aclocal.m4", "config.h.in", "configure", "Makefile.in", "sub/Makefile.in"}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	if err := TouchAutotools(dir); err != nil {
		t.Fatal(err)
	}
	mt := func(f string) time.Time {
		fi, _ := os.Stat(filepath.Join(dir, f))
		return fi.ModTime()
	}
	// ascending tiers: inputs < aclocal.m4 < configure < Makefile.in
	if !mt("configure.ac").Before(mt("aclocal.m4")) ||
		!mt("aclocal.m4").Before(mt("configure")) ||
		!mt("configure").Before(mt("Makefile.in")) ||
		!mt("configure").Before(mt("sub/Makefile.in")) {
		t.Errorf("mtimes not ascending: ac=%v aclocal=%v configure=%v mk=%v",
			mt("configure.ac"), mt("aclocal.m4"), mt("configure"), mt("Makefile.in"))
	}
}

func TestTouchAutotoolsErrors(t *testing.T) {
	// walk error: a non-existent dir
	if err := TouchAutotools(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected walk error on missing dir")
	}
	// chtimes error via the seam
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "configure.ac"), []byte("x"), 0o644)
	old := osChtimes
	defer func() { osChtimes = old }()
	osChtimes = func(string, time.Time, time.Time) error { return os.ErrPermission }
	if err := TouchAutotools(dir); err == nil {
		t.Error("expected chtimes seam error")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func sortedStrings(ss []string) bool {
	for i := 1; i < len(ss); i++ {
		if ss[i-1] > ss[i] {
			return false
		}
	}
	return true
}
