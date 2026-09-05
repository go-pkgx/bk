package build

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	want := []string{"arch.only@2", "gnu.org/A", "openssl.org^1.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linux DepSpecs = %v want %v", got, want)
	}
	// windows/x86-64 target: openssl + windows sub (bare) + x86-64 sub
	got = DepSpecs(deps, win())
	want = []string{"arch.only@2", "llvm.org/mingw-w64", "openssl.org^1.1"}
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

// TestDepSpecConstraintShapes exercises every rendering branch: range operators
// (^ ~ > <) append directly, exact = strips to @, bare numeric keeps @, star is
// bare. These match how pkgx v2.10.3 actually parses each form.
func TestDepSpecConstraintShapes(t *testing.T) {
	cases := []struct {
		project, constraint, want string
	}{
		{"cmake.org", "^3", "cmake.org^3"},
		{"cmake.org", "~3.31", "cmake.org~3.31"},
		{"gnu.org/gmp", ">=6", "gnu.org/gmp>=6"},
		{"foo", "<2", "foo<2"},
		{"foo", "3", "foo@3"},
		{"bar", "=1.2.3", "bar@1.2.3"},
		{"baz", "*", "baz"},
		{"qux", "", "qux"},
	}
	for _, c := range cases {
		if got := depSpec(c.project, c.constraint); got != c.want {
			t.Errorf("depSpec(%q,%q)=%q want %q", c.project, c.constraint, got, c.want)
		}
		if got := DepSpecs(map[string]any{c.project: c.constraint}, lin()); len(got) != 1 || got[0] != c.want {
			t.Errorf("DepSpecs %q:%q = %v want [%q]", c.project, c.constraint, got, c.want)
		}
	}
}

// TestSpecProject checks project extraction across every delimiter form so that
// EvalDeps dedup keys are stable regardless of constraint shape.
func TestSpecProject(t *testing.T) {
	for spec, want := range map[string]string{
		"cmake.org^3":    "cmake.org",
		"foo@3":          "foo",
		"gnu.org/gmp>=6": "gnu.org/gmp",
		"bar~1.2":        "bar",
		"baz<2":          "baz",
		"qux=1":          "qux",
		"plain":          "plain",
	} {
		if got := SpecProject(spec); got != want {
			t.Errorf("SpecProject(%q)=%q want %q", spec, got, want)
		}
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
	// gawk is pinned to 5.3 (5.4.1 regressed autotools config-header generation,
	// e.g. libpng's pnglibconf drops PNG_SETJMP_SUPPORTED). Guard against a revert
	// to the bare/latest form, and confirm it still dedups by bare project.
	if contains(base, "gnu.org/gawk") || !contains(base, "gnu.org/gawk@5.3") {
		t.Errorf("gawk must be pinned to @5.3, got base = %v", base)
	}
	// bison is required so autotools packages that ship a .y grammar (gettext's
	// plural.y) can regenerate the .c during the build (ylwrap → bison).
	if !contains(base, "gnu.org/bison") {
		t.Errorf("base toolchain must include gnu.org/bison, got %v", base)
	}
	if SpecProject("gnu.org/gawk@5.3") != "gnu.org/gawk" {
		t.Errorf("SpecProject should strip @5.3 for dedup")
	}
	// EvalDeps = base + runtime + build, deduped by project, sorted
	got := EvalDeps("acme.org/thing",
		map[string]any{"openssl.org": "^1.1"},
		map[string]any{"freedesktop.org/pkg-config": "^0.29"}, // already in base → dedup
		lin(),
	)
	if !contains(got, "openssl.org^1.1") || !contains(got, "gnu.org/autoconf") {
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

func TestDepTokens(t *testing.T) {
	resolve := func(p, c string) (string, error) { return "1.2.3", nil }
	toks, err := DepTokens(
		map[string]any{"openssl.org": "^1.1"},
		map[string]any{"freedesktop.org/pkg-config": "^0.29"},
		lin(), "/opt/pkgx", resolve)
	if err != nil {
		t.Fatal(err)
	}
	find := func(from string) (string, bool) {
		for _, tk := range toks {
			if tk.From == from {
				return tk.To, true
			}
		}
		return "", false
	}
	if to, ok := find("deps.openssl.org.prefix"); !ok || to != filepath.FromSlash("/opt/pkgx/openssl.org/v1.2.3") {
		t.Errorf("openssl prefix = %q %v", to, ok)
	}
	if to, _ := find("deps.openssl.org.version"); to != "1.2.3" {
		t.Errorf("openssl version = %q", to)
	}
	if _, ok := find("deps.freedesktop.org/pkg-config.prefix"); !ok {
		t.Error("build dep pkg-config token missing")
	}
	// resolve error propagates
	if _, err := DepTokens(map[string]any{"x": "1"}, nil, lin(), "/p", func(string, string) (string, error) { return "", errBoom }); err == nil {
		t.Error("expected resolve error")
	}
	// empty deps → no tokens
	if tk, _ := DepTokens(nil, nil, lin(), "/p", resolve); len(tk) != 0 {
		t.Errorf("empty = %v", tk)
	}
}

func TestSanitizedEnv(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("GITHUB_TOKEN", "secret")
	// Where the build's own `pkgx +deps` resolves from: a local registry cache
	// and a corrected overlay only apply if they survive the sanitised env.
	t.Setenv("PKGX_DIST", "oci://http://cache:5111/go-pkgx/packages")
	t.Setenv("PKGX_PANTRY", "https://pantry.example/projects")
	t.Setenv("PKGX_PANTRY_OVERLAY", "https://overlay.example/projects")
	// The opt-out a test harness needs when it points at unsigned fixtures.
	t.Setenv("PKGX_VERIFY", "0")
	os.Unsetenv("PKGX_PANTRY_DIR")
	env := SanitizedEnv("/h", "/pkgx")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=/h", "PKGX_DIR=/pkgx", "TERM=xterm", "GITHUB_TOKEN=secret", "MAKEFLAGS=ACLOCAL=true AUTOMAKE=true AUTOCONF=true AUTOHEADER=true AUTOPOINT=true", "FORCE_UNSAFE_CONFIGURE=1", "PKGX_DIST=oci://http://cache:5111/go-pkgx/packages", "PKGX_PANTRY=https://pantry.example/projects", "PKGX_PANTRY_OVERLAY=https://overlay.example/projects", "PKGX_VERIFY=0"} {
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
	files := []string{"configure.ac", "acinclude.m4", "m4/pkg.m4", "aclocal.m4", "config.h.in", "configure", "Makefile.in", "sub/Makefile.in"}
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
	// ascending tiers: inputs (incl. m4/ macros) < aclocal.m4 < configure < Makefile.in
	if !mt("m4/pkg.m4").Before(mt("aclocal.m4")) {
		t.Errorf("m4/ macro not aged before aclocal.m4: m4=%v aclocal=%v", mt("m4/pkg.m4"), mt("aclocal.m4"))
	}
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

// TestEvalDepsRecipeConstraintBeatsBase: the base toolchain is a floor, not a
// pin. gnu.org/texinfo declares `perl.org: ~5.42` because its XS modules are
// compiled against that perl; when the base's unconstrained perl.org won, every
// recipe running makeinfo died with "Perl API version v5.42.0 … does not match
// v5.44.0". A recipe must be able to constrain a base project.
func TestEvalDepsRecipeConstraintBeatsBase(t *testing.T) {
	got := EvalDeps("acme.org/thing", map[string]any{"perl.org": "~5.42"}, nil, lin())
	var perl []string
	for _, s := range got {
		if SpecProject(s) == "perl.org" {
			perl = append(perl, s)
		}
	}
	if len(perl) != 1 || perl[0] != "perl.org~5.42" {
		t.Fatalf("perl specs = %v, want exactly [perl.org~5.42]", perl)
	}
	// a BUILD dep constrains it just as well
	got = EvalDeps("acme.org/thing", nil, map[string]any{"gnu.org/gawk": "^5.4"}, lin())
	if !contains(got, "gnu.org/gawk^5.4") || contains(got, "gnu.org/gawk@5.3") {
		t.Errorf("a build dep must override the base's gawk pin: %v", got)
	}
	// …and a project the recipe says nothing about still comes from the base,
	// with the base's own pin intact.
	if !contains(EvalDeps("acme.org/thing", nil, nil, lin()), "gnu.org/gawk@5.3") {
		t.Error("the base pin must survive when the recipe is silent")
	}
}

// TestBaseToolchainPerlMatchesTexinfo: the base toolchain and the tools inside
// it have to agree. texinfo ships compiled XS modules and asks for perl ~5.42;
// an unconstrained perl put 5.44 on PATH ahead of it and makeinfo refused to
// load its own modules, taking down every recipe that generates a .info manual.
func TestBaseToolchainPerlMatchesTexinfo(t *testing.T) {
	base := BaseToolchain()
	if !contains(base, "perl.org~5.42") || contains(base, "perl.org") {
		t.Errorf("perl must be pinned to texinfo's line, got %v", base)
	}
	// a recipe still wins over the pin
	if got := EvalDeps("acme.org/thing", map[string]any{"perl.org": "^5.44"}, nil, lin()); !contains(got, "perl.org^5.44") || contains(got, "perl.org~5.42") {
		t.Errorf("a recipe must override the base perl pin: %v", got)
	}
}

// TestBaseToolchainHasHelp2man: autotools recipes generate man pages with
// help2man through build-aux/missing. It has to be in the EVAL, not merely on
// PATH: pkgx only exports a package's runtime env for packages in the closure,
// and help2man publishes the PERL5LIB that finds the Locale::gettext it bundles.
// Without that, help2man runs and dies with "Can't locate Locale/gettext.pm".
func TestBaseToolchainHasHelp2man(t *testing.T) {
	if !contains(BaseToolchain(), "gnu.org/help2man") {
		t.Errorf("base toolchain must include gnu.org/help2man, got %v", BaseToolchain())
	}
}

// TestSanitizedEnvPassesCacheAndQemu: two variables that must survive the
// sanitised environment, each for a reason that was measured rather than
// assumed.
//
// PKGX_CACHE names a distribution point, like PKGX_DIST beside it: dropping it
// sends the `pkgx +deps` line of every generated script past the pull-through
// cache the job was configured to use.
//
// QEMU_RESERVED_VA matters when the target is emulated. Our gnu.org/bash is
// built with bash's own sbrk-based malloc, and under qemu-user's default
// reserved address space it cannot allocate at all —
//
//	/bin/sh: xmalloc: setlinebuf.c:51: cannot allocate 2016 bytes (0 bytes allocated)
//
// on the first command of the build. In an emulated container our bash fails,
// the same bash with QEMU_RESERVED_VA set succeeds, and the distribution's bash
// (built --without-bash-malloc) never had the problem.
func TestSanitizedEnvPassesCacheAndQemu(t *testing.T) {
	t.Setenv("PKGX_CACHE", "oci://http://10.0.0.1:5111/go-pkgx/packages")
	t.Setenv("QEMU_RESERVED_VA", "0x100000000")
	env := SanitizedEnv("/root", "/pkgx")
	for _, want := range []string{
		"PKGX_CACHE=oci://http://10.0.0.1:5111/go-pkgx/packages",
		"QEMU_RESERVED_VA=0x100000000",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("sanitised env dropped %q", want)
		}
	}
	// The guard the allow-list exists for is unchanged: ambient host toolchain
	// state still does not travel.
	t.Setenv("HOMEBREW_PREFIX", "/opt/homebrew")
	for _, got := range SanitizedEnv("/root", "/pkgx") {
		if strings.HasPrefix(got, "HOMEBREW_PREFIX=") {
			t.Error("sanitised env leaked HOMEBREW_PREFIX")
		}
	}
}

// A package's own published bottle must not join the toolchain of its own
// build. It would shadow the thing being built with an older copy — and when
// that copy cannot start, the package can no longer be repaired at all:
// gnu.org/grep's own configure refuses with "no working 'grep' found", so the
// broken bottle blocked the build that would have replaced it.
func TestOwnBottleIsNotInItsOwnToolchain(t *testing.T) {
	got := EvalDeps("gnu.org/grep", nil, nil, lin())
	for _, s := range got {
		if SpecProject(s) == "gnu.org/grep" {
			t.Fatalf("grep's own build got grep from the base toolchain: %v", got)
		}
	}
	// The rest of the floor is untouched — this removes one entry, not the list.
	if !contains(got, "gnu.org/sed") || !contains(got, "gnu.org/coreutils") {
		t.Errorf("the base toolchain lost more than the package itself: %v", got)
	}
	// And another package still gets grep.
	other := EvalDeps("acme.org/thing", nil, nil, lin())
	if !contains(other, "gnu.org/grep") {
		t.Errorf("grep vanished from an unrelated build: %v", other)
	}
}

// A recipe that names itself is stating something we have no business
// overruling: only the base toolchain is filtered.
func TestARecipeMayStillNameItself(t *testing.T) {
	got := EvalDeps("gnu.org/grep", nil, map[string]any{"gnu.org/grep": "^3"}, lin())
	if !contains(got, "gnu.org/grep^3") {
		t.Errorf("a recipe's own explicit build dep was dropped: %v", got)
	}
}
