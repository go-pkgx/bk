package buildscript

import (
	"strings"
	"testing"

	"github.com/go-pkgx/bk/target"
)

func linuxTgt() target.Target  { return target.Target{Platform: "linux", Arch: "x86-64"} }
func darwinTgt() target.Target { return target.Target{Platform: "darwin", Arch: "aarch64"} }
func winTgt() target.Target    { return target.Target{Platform: "windows", Arch: "x86-64"} }

func TestWrapLinux(t *testing.T) {
	s := Wrap(WrapOptions{
		UserScript: "./configure\nmake install\n",
		Deps:       []string{"openssl.org@1.1", "zlib.net"},
		Target:     linuxTgt(), Host: linuxTgt(),
		Home: "/bk/home", SrcRoot: "/bk/build", PkgxDir: "/opt/pkgx",
		BrewkitPath: "/bk/libexec", PkgxBin: "/opt/pkgx/bin/pkgx", BashPath: "/opt/bash",
	})
	wants := []string{
		"#!/opt/bash\n",
		"set -eo pipefail",
		`eval "$(CLICOLOR_FORCE=1 /opt/pkgx/bin/pkgx "+openssl.org@1.1" "+zlib.net" "+llvm.org")"`,
		`export PATH="/bk/libexec:$PATH"`,
		`export CMAKE_PREFIX_PATH="/opt/pkgx${CMAKE_PREFIX_PATH:+:$CMAKE_PREFIX_PATH}"`,
		`export PKGX="/opt/pkgx/bin/pkgx"`,
		`export HOME="/bk/home"`,
		`export SRCROOT="/bk/build"`,
		`export TMPDIR="$HOME/tmp"; mkdir -p "$TMPDIR"`,
		"export FORCE_UNSAFE_CONFIGURE=1",
		`export LDFLAGS="-pie -Wl,-rpath,/opt/pkgx $LDFLAGS"`,
		`export CFLAGS="-fPIC -Wno-implicit-function-declaration -Wno-implicit-int -Wno-int-conversion $CFLAGS"`,
		`export CXXFLAGS="-fPIC $CXXFLAGS"`,
		"env -u GH_TOKEN -u GITHUB_TOKEN",
		`cd "/bk/build"`,
		"./configure\nmake install",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
	// linux gets an rpath slot (relocated $ORIGIN-relative by fixup) but no
	// darwin-only MACOSX_DEPLOYMENT_TARGET.
	if strings.Contains(s, "MACOSX_DEPLOYMENT_TARGET") {
		t.Error("linux script has darwin flags")
	}
}

func TestWrapLinuxArm64Flags(t *testing.T) {
	// linux/aarch64: the C23-relaxation CFLAGS apply, but without -fPIC (default
	// on arm64) and without the x86-64 -pie LDFLAGS / -fPIC CXXFLAGS.
	s := Wrap(WrapOptions{
		UserScript: "make", Deps: []string{"x"},
		Target:  target.Target{Platform: "linux", Arch: "aarch64"},
		Host:    target.Target{Platform: "linux", Arch: "aarch64"},
		PkgxDir: "/opt/pkgx",
	})
	if !strings.Contains(s, `export CFLAGS="-Wno-implicit-function-declaration -Wno-implicit-int -Wno-int-conversion $CFLAGS"`) {
		t.Errorf("arm64 CFLAGS wrong:\n%s", s)
	}
	// aarch64 links PIE by default: rpath slot, but no -pie / -fPIC / CXXFLAGS.
	if !strings.Contains(s, `export LDFLAGS="-Wl,-rpath,/opt/pkgx $LDFLAGS"`) {
		t.Errorf("arm64 LDFLAGS should carry the rpath slot only:\n%s", s)
	}
	if strings.Contains(s, "-fPIC") || strings.Contains(s, "-pie") || strings.Contains(s, "CXXFLAGS") {
		t.Errorf("arm64 got x86-64-only flags:\n%s", s)
	}
}

func TestWrapDarwinWithBinutils(t *testing.T) {
	s := Wrap(WrapOptions{
		UserScript: "make", Deps: []string{"x"}, Target: darwinTgt(), Host: darwinTgt(),
		PkgxDir: "/opt/pkgx", HasBinutils: true,
	})
	// darwin host => NO default llvm.org appended
	if strings.Contains(s, "+llvm.org") {
		t.Error("darwin host should not add default llvm.org")
	}
	for _, w := range []string{
		`export LDFLAGS="-Wl,-rpath,/opt/pkgx $LDFLAGS"`,
		"export MACOSX_DEPLOYMENT_TARGET=11.0",
		"export AR=/usr/bin/ar",
		"export RANLIB=/usr/bin/ranlib",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q", w)
		}
	}
	// no -pie/-fPIC on darwin
	if strings.Contains(s, "-pie") || strings.Contains(s, "-fPIC") {
		t.Error("darwin got linux flags")
	}
}

func TestWrapWindowsCross(t *testing.T) {
	// windows target on a linux host: no default llvm.org, no FLAGS; TMPDIR is
	// host-based (linux) so TMPDIR not TMP.
	s := Wrap(WrapOptions{
		UserScript: "make", Deps: []string{"llvm.org/mingw-w64"},
		Target: winTgt(), Host: linuxTgt(), Home: "/h", SrcRoot: "/s",
	})
	if strings.Contains(s, `"+llvm.org"`) {
		t.Error("windows target must not add default llvm.org")
	}
	if strings.Contains(s, "LDFLAGS") || strings.Contains(s, "-pie") || strings.Contains(s, "rpath") {
		t.Errorf("windows target should have no FLAGS:\n%s", s)
	}
	if !strings.Contains(s, `export TMPDIR="$HOME/tmp"`) {
		t.Error("linux host => TMPDIR")
	}
}

func TestWrapWindowsHostTmp(t *testing.T) {
	s := Wrap(WrapOptions{UserScript: "x", Host: winTgt(), Target: winTgt()})
	if !strings.Contains(s, `export TMP="$HOME/tmp"; export TEMP="$HOME/tmp"`) {
		t.Errorf("windows host => TMP/TEMP: %s", s)
	}
}

func TestWrapHasCompilerAndDefaults(t *testing.T) {
	// HasCompiler suppresses the default llvm.org even on a linux host
	s := Wrap(WrapOptions{UserScript: "x", Deps: []string{"gnu.org/gcc"}, Target: linuxTgt(), Host: linuxTgt(), HasCompiler: true})
	if strings.Contains(s, "+llvm.org") {
		t.Error("HasCompiler should suppress default llvm.org")
	}
	// defaults: no deps => no eval block; default bash + pkgx
	s2 := Wrap(WrapOptions{UserScript: "x", Target: darwinTgt(), Host: darwinTgt()})
	if strings.Contains(s2, "eval \"$(") {
		t.Error("no deps + darwin host => no eval block")
	}
	if !strings.HasPrefix(s2, "#!/bin/bash\n") {
		t.Error("default bash shebang")
	}
	// no-deps linux still gets the llvm.org default => an eval block appears
	s3 := Wrap(WrapOptions{UserScript: "x", Target: linuxTgt(), Host: linuxTgt()})
	if !strings.Contains(s3, `eval "$(CLICOLOR_FORCE=1 pkgx "+llvm.org")"`) {
		t.Errorf("no-dep linux should still eval default llvm.org via `pkgx`: %s", s3)
	}
}
