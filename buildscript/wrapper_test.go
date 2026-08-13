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
		`export LDFLAGS="-Wl,-rpath,/opt/pkgx $LDFLAGS"`,
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

func linuxArm64Tgt() target.Target { return target.Target{Platform: "linux", Arch: "aarch64"} }

func TestWrapLibcPkgxLinuxX86(t *testing.T) {
	s := Wrap(WrapOptions{
		UserScript: "make install\n",
		Deps:       []string{"zlib.net"},
		Target:     linuxTgt(), Host: linuxTgt(),
		Home: "/bk/home", SrcRoot: "/bk/build", PkgxDir: "/opt/pkgx",
		PkgxBin: "/opt/pkgx/bin/pkgx", LibcPkgx: true,
	})
	wants := []string{
		// glibc + kernel-headers + binutils + libcxx bottles join the eval.
		`"+zlib.net" "+llvm.org" "+gnu.org/glibc" "+kernel.org/linux-headers" "+gnu.org/binutils" "+libcxx.llvm.org"`,
		// build-time resolution of each bottle prefix (noglob-safe subshell glob).
		`export BK_GLIBC_PREFIX="$(set +f; printf '%s\n' "$PKGX_DIR"/gnu.org/glibc/v[0-9]*/ | sort -V | tail -n1)"`,
		`export BK_GLIBC_LIB="$(set +f; printf '%s\n' "${BK_GLIBC_PREFIX}"lib/glibc-[0-9]*/ | sort -V | tail -n1)"`,
		`export BK_KHDR_PREFIX="$(set +f; printf '%s\n' "$PKGX_DIR"/kernel.org/linux-headers/v[0-9]*/ | sort -V | tail -n1)"`,
		`export BK_LIBCXX_PREFIX="$(set +f; printf '%s\n' "$PKGX_DIR"/libcxx.llvm.org/v[0-9]*/ | sort -V | tail -n1)"`,
		// C: glibc sysroot + compiler-rt, no unwinder (exception-free C).
		`export CFLAGS="-fPIC --sysroot="$BK_GLIBC_PREFIX" -isystem "${BK_GLIBC_PREFIX}include" -isystem "${BK_KHDR_PREFIX}include" -B "$BK_GLIBC_LIB" -L "$BK_GLIBC_LIB" --rtlib=compiler-rt -fuse-ld=lld --unwindlib=none -Wno-implicit-function-declaration`,
		// C++: libc++ headers FIRST (before glibc's), then sysroot + libunwind.
		`export CXXFLAGS="-stdlib=libc++ -isystem "${BK_LIBCXX_PREFIX}include/c++/v1" --sysroot="$BK_GLIBC_PREFIX" -isystem "${BK_GLIBC_PREFIX}include" -isystem "${BK_KHDR_PREFIX}include" -B "$BK_GLIBC_LIB" -L "$BK_GLIBC_LIB" --rtlib=compiler-rt -fuse-ld=lld --unwindlib=libunwind -fPIC`,
		// PT_INTERP = the bottle's x86-64 loader; libc + libc++/libunwind lib search paths; DT_RPATH.
		`-Wl,--dynamic-linker="${BK_GLIBC_LIB}ld-linux-x86-64.so.2" -Wl,-rpath,"$BK_GLIBC_LIB" -L "${BK_LIBCXX_PREFIX}lib" -Wl,-rpath,"${BK_LIBCXX_PREFIX}lib" -Wl,--disable-new-dtags`,
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
	// The C23 relax + own-lib rpath still apply alongside the glibc targeting.
	if !strings.Contains(s, "-Wno-implicit-function-declaration") || !strings.Contains(s, "-Wl,-rpath,/opt/pkgx") {
		t.Errorf("base linux flags dropped in pkgx-libc mode:\n%s", s)
	}
}

func TestWrapLibcPkgxArm64Loader(t *testing.T) {
	s := Wrap(WrapOptions{
		UserScript: "make\n", Target: linuxArm64Tgt(), Host: linuxArm64Tgt(),
		PkgxDir: "/opt/pkgx", LibcPkgx: true,
	})
	if !strings.Contains(s, `ld-linux-aarch64.so.1`) {
		t.Errorf("arm64 loader wrong:\n%s", s)
	}
	if strings.Contains(s, "ld-linux-x86-64") || strings.Contains(s, "-fPIC") {
		t.Errorf("arm64 script leaked x86-64 flags:\n%s", s)
	}
	// arm64 CXXFLAGS leads with libc++ then the glibc sysroot (non-fPIC branch).
	if !strings.Contains(s, `export CXXFLAGS="-stdlib=libc++ -isystem "${BK_LIBCXX_PREFIX}include/c++/v1" --sysroot="$BK_GLIBC_PREFIX"`) {
		t.Errorf("arm64 CXXFLAGS missing libc++/sysroot:\n%s", s)
	}
}

func TestWrapLibcPkgxNonLinuxIgnored(t *testing.T) {
	// LibcPkgx only affects linux: darwin gets neither the glibc dep nor flags.
	s := Wrap(WrapOptions{
		UserScript: "make\n", Target: darwinTgt(), Host: linuxTgt(), LibcPkgx: true,
	})
	if strings.Contains(s, "gnu.org/glibc") || strings.Contains(s, "BK_GLIBC") || strings.Contains(s, "--dynamic-linker") {
		t.Errorf("darwin target must ignore LibcPkgx:\n%s", s)
	}
}

func TestGlibcLoader(t *testing.T) {
	for _, c := range []struct{ arch, want string }{
		{"x86-64", "ld-linux-x86-64.so.2"},
		{"aarch64", "ld-linux-aarch64.so.1"},
		{"arm64", "ld-linux-aarch64.so.1"},
	} {
		if got := glibcLoader(c.arch); got != c.want {
			t.Errorf("glibcLoader(%q)=%q want %q", c.arch, got, c.want)
		}
	}
}

// TestWrapLibcPkgxGlibcPin: -glibc pins the exact glibc version in the eval.
func TestWrapLibcPkgxGlibcPin(t *testing.T) {
	for _, in := range []string{"2.27.0", "=2.27.0"} {
		s := Wrap(WrapOptions{
			UserScript: "make install\n",
			Target:     linuxTgt(), Host: linuxTgt(),
			Home: "/bk/home", SrcRoot: "/bk/build", PkgxDir: "/opt/pkgx",
			PkgxBin: "/opt/pkgx/bin/pkgx", LibcPkgx: true, Glibc: in,
		})
		if !strings.Contains(s, `"+gnu.org/glibc@=2.27.0"`) {
			t.Errorf("glibc=%q: missing pinned +gnu.org/glibc@=2.27.0 in:\n%s", in, s)
		}
		if strings.Contains(s, `"+gnu.org/glibc" `) {
			t.Errorf("glibc=%q: unpinned glibc spec present", in)
		}
	}
	// no pin -> unversioned (newest)
	s := Wrap(WrapOptions{
		Target: linuxTgt(), Host: linuxTgt(), Home: "/h", SrcRoot: "/b",
		PkgxDir: "/opt/pkgx", PkgxBin: "/opt/pkgx/bin/pkgx", LibcPkgx: true,
	})
	if !strings.Contains(s, `"+gnu.org/glibc"`) || strings.Contains(s, "glibc@=") {
		t.Errorf("no-pin should use unversioned glibc:\n%s", s)
	}
}
