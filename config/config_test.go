package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/target"
)

var errNoHome = errors.New("no home")

func TestPkgxDir(t *testing.T) {
	t.Setenv("PKGX_DIR", "/opt/pkgx")
	if PkgxDir() != "/opt/pkgx" {
		t.Errorf("PkgxDir env = %q", PkgxDir())
	}
	t.Setenv("PKGX_DIR", "")
	got := PkgxDir()
	if !strings.HasSuffix(got, ".pkgx") {
		t.Errorf("PkgxDir default = %q", got)
	}
}

func TestComputeDefaultLayout(t *testing.T) {
	t.Setenv("PKGX_PANTRY_DIR", "")
	t.Setenv("PKGX_PANTRY_PATH", "")
	t.Setenv("PKGX_DIR", "/opt/pkgx")
	t.Setenv("XDG_DATA_HOME", "/data")

	tgt := target.Target{Platform: "windows", Arch: "x86-64", Triple: "x86_64-w64-mingw32"}
	p := Compute("acme.org/tool", "1.2.3", tgt)

	if p.Install != filepath.FromSlash("/opt/pkgx/acme.org/tool/v1.2.3") {
		t.Errorf("Install = %q", p.Install)
	}
	if p.BuildInstall != p.Install+"+brewing" {
		t.Errorf("BuildInstall = %q", p.BuildInstall)
	}
	// keyed on the TARGET platform+arch
	wantRoot := filepath.FromSlash("/data/brewkit/windows+x86-64/acme.org/tool/v1.2.3")
	if p.Home != wantRoot {
		t.Errorf("Home = %q want %q", p.Home, wantRoot)
	}
	if p.Src != filepath.Join(wantRoot, "src") || p.Build != filepath.Join(wantRoot, "build") || p.Test != filepath.Join(wantRoot, "testbed") {
		t.Errorf("paths = %+v", p)
	}
	if p.TarballDir != filepath.FromSlash("/data/brewkit") {
		t.Errorf("TarballDir = %q", p.TarballDir)
	}

	// a native build for a different target gets a different tree
	q := Compute("acme.org/tool", "1.2.3", target.Target{Platform: "linux", Arch: "aarch64"})
	if q.Home == p.Home {
		t.Error("cross and native builds must not share a tree")
	}
}

func TestComputePantryCheckoutLayout(t *testing.T) {
	t.Setenv("PKGX_DIR", "/opt/pkgx")
	t.Setenv("PKGX_PANTRY_DIR", "/co")
	p := Compute("acme.org/tool", "2.0.0", target.Host())
	if p.Home != filepath.FromSlash("/co/homes/acme.org__tool-2.0.0") {
		t.Errorf("checkout Home = %q", p.Home)
	}
	if p.Src != filepath.FromSlash("/co/srcs/acme.org__tool-2.0.0") {
		t.Errorf("checkout Src = %q", p.Src)
	}
	if p.Build != filepath.FromSlash("/co/builds/acme.org__tool-2.0.0") {
		t.Errorf("checkout Build = %q", p.Build)
	}
	if p.Test != filepath.FromSlash("/co/testbeds/acme.org__tool-2.0.0") {
		t.Errorf("checkout Test = %q", p.Test)
	}
	if p.TarballDir != filepath.FromSlash("/co/srcs") {
		t.Errorf("checkout TarballDir = %q", p.TarballDir)
	}
	// the older PKGX_PANTRY_PATH env is honoured too
	t.Setenv("PKGX_PANTRY_DIR", "")
	t.Setenv("PKGX_PANTRY_PATH", "/co2")
	if q := Compute("x", "1", target.Host()); q.Home != filepath.FromSlash("/co2/homes/x-1") {
		t.Errorf("PKGX_PANTRY_PATH Home = %q", q.Home)
	}
}

func TestNoHomeFallbacks(t *testing.T) {
	orig := userHomeDir
	defer func() { userHomeDir = orig }()
	userHomeDir = func() (string, error) { return "", errNoHome }

	t.Setenv("PKGX_DIR", "")
	if got := PkgxDir(); got != ".pkgx" {
		t.Errorf("PkgxDir no-home = %q", got)
	}
	t.Setenv("XDG_DATA_HOME", "")
	if got := dataHome(); got != filepath.Join(".local", "share") {
		t.Errorf("dataHome no-home = %q", got)
	}
}

func TestDataHomeVariants(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if dataHome() != "/xdg" {
		t.Errorf("XDG_DATA_HOME = %q", dataHome())
	}
	t.Setenv("XDG_DATA_HOME", "")
	// the runtime wrapper matches the pure fn for the current host
	if dataHome() != dataHomeFor(runtime.GOOS, mustHome(t)) {
		t.Errorf("dataHome != dataHomeFor(host)")
	}
	// both OS branches, pure
	if dataHomeFor("darwin", "/h") != filepath.FromSlash("/h/Library/Application Support") {
		t.Errorf("darwin: %q", dataHomeFor("darwin", "/h"))
	}
	if dataHomeFor("linux", "/h") != filepath.FromSlash("/h/.local/share") {
		t.Errorf("linux: %q", dataHomeFor("linux", "/h"))
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	h, err := userHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
