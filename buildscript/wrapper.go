package buildscript

import (
	"fmt"
	"strings"

	"github.com/go-pkgx/bk/target"
)

// WrapOptions carries the environment the porcelain wrapper sets up around the
// user script (the output of Generate). It is a port of libpkgx brewkit's
// make_build_script.
type WrapOptions struct {
	UserScript  string        // the pantry script, already rendered by Generate
	Deps        []string      // dependency pkgspecs for `pkgx +<spec>` (runtime + build)
	Target      target.Target // what we build FOR — drives FLAGS
	Host        target.Target // where we run — drives TMPDIR
	Home        string        // a fresh HOME for the build
	SrcRoot     string        // the build directory (also SRCROOT / cd target)
	PkgxDir     string        // $PKGX_DIR — the rpath root and CMAKE_PREFIX_PATH
	BrewkitPath string        // dir prepended to PATH for the build shims (optional)
	PkgxBin     string        // path to the pkgx binary (for the deps eval + $PKGX)
	BashPath    string        // shebang interpreter (default /bin/bash)
	HasCompiler bool          // a compiler (llvm.org / gnu.org/gcc) is already a dep
	HasBinutils bool          // gnu.org/binutils is a dep (darwin AR/RANLIB workaround)
	// PkgxLibc, when true, emits the EXPERIMENTAL "Isolation Phase B" block that
	// writes a clang.cfg re-targeting clang at the pkgx gnu.org/glibc bottle, so
	// produced binaries carry the pkgx ld-linux as their PT_INTERP instead of the
	// build host's loader (see docs/from-scratch-toolchain.md). OFF by default;
	// the emitted shell is itself additionally gated on $BK_PKGX_LIBC at run time,
	// and only fires for a native (Target.Arch == Host.Arch) linux target.
	PkgxLibc bool
}

// LibcToolchain is the set of implicit toolchain bottles the PkgxLibc mechanism
// needs on the `pkgx +…` closure (clang itself comes from llvm.org, already
// added by depPlus): the relocatable glibc (crt + libc + ld-linux) and the gcc
// bottle that carries libgcc_s.so.1 (the unwinder). EXPERIMENTAL — a caller
// wires this into the dep closure only under BK_PKGX_LIBC; it is not yet added
// by build.EvalDeps.
func LibcToolchain() []string {
	return []string{"gnu.org/glibc", "gnu.org/gcc/libstdcxx"}
}

// Wrap assembles the complete, runnable build script: a sanitized env, the
// dependency environment (`eval "$(pkgx +…)"`), target-specific compiler/linker
// FLAGS, a per-host TMPDIR, then the user script run from SRCROOT.
func Wrap(o WrapOptions) string {
	bash := o.BashPath
	if bash == "" {
		bash = "/bin/bash"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#!%s\n\nset -eo pipefail\n\n", bash)
	b.WriteString("export PKGX_HOME=\"$HOME\"\n")

	if plus := o.depPlus(); plus != "" {
		pkgx := o.PkgxBin
		if pkgx == "" {
			pkgx = "pkgx"
		}
		b.WriteString("set -a\n")
		fmt.Fprintf(&b, "eval \"$(CLICOLOR_FORCE=1 %s %s)\"\n", pkgx, plus)
		b.WriteString("set +a\n")
	}
	if o.BrewkitPath != "" {
		fmt.Fprintf(&b, "export PATH=\"%s:$PATH\"\n", o.BrewkitPath)
	}
	if o.PkgxDir != "" {
		fmt.Fprintf(&b, "export CMAKE_PREFIX_PATH=\"%s${CMAKE_PREFIX_PATH:+:$CMAKE_PREFIX_PATH}\"\n", o.PkgxDir)
	}
	if o.PkgxBin != "" {
		fmt.Fprintf(&b, "export PKGX=\"%s\"\n", o.PkgxBin)
	}
	fmt.Fprintf(&b, "export HOME=%q\n", o.Home)
	fmt.Fprintf(&b, "export SRCROOT=%q\n", o.SrcRoot)
	b.WriteString(tmpdirLine(o.Host) + "\n")
	b.WriteString("if [ -n \"$CI\" ]; then export FORCE_UNSAFE_CONFIGURE=1; fi\n")
	b.WriteString("mkdir -p $HOME\n")
	for _, f := range wrapFlags(o.Target, o.PkgxDir, o.HasBinutils) {
		b.WriteString(f + "\n")
	}
	if blk := pkgxLibcBlock(o); blk != "" {
		b.WriteString(blk)
	}
	b.WriteString("env -u GH_TOKEN -u GITHUB_TOKEN\n\n")

	b.WriteString("set -x\n")
	fmt.Fprintf(&b, "cd %q\n\n", o.SrcRoot)
	b.WriteString(strings.TrimRight(o.UserScript, "\n"))
	b.WriteString("\n")
	return b.String()
}

// depPlus renders the `+spec` dependency list for the pkgx env eval, adding
// llvm.org as the default compiler unless one is already present, the host is
// darwin (uses the system toolchain), or the target is a windows cross build
// (its compiler comes from the recipe's llvm-mingw dep).
func (o WrapOptions) depPlus() string {
	parts := make([]string, 0, len(o.Deps)+1)
	for _, d := range o.Deps {
		parts = append(parts, `"+`+d+`"`)
	}
	if !o.HasCompiler && o.Target.Platform != "windows" && o.Host.Platform != "darwin" {
		parts = append(parts, `"+llvm.org"`)
	}
	return strings.Join(parts, " ")
}

// pkgxLibcBlock renders the EXPERIMENTAL clang.cfg-generation shell for the
// PkgxLibc switch. It returns "" (no output) unless PkgxLibc is set for a native
// linux target — a windows target (PE, no ELF interpreter) and a cross-arch
// build (the loader must be the TARGET arch's, not this host's) are skipped, so
// the mechanism is inert everywhere it cannot be correct. The emitted shell is
// itself guarded on $BK_PKGX_LIBC so it is a runtime no-op unless the operator
// opts in, and it locates the pkgx glibc/gcc bottles + the real clang purely
// from the already-eval'd $PKGX_DIR (globbing the pkgx layout), then writes a
// clang.cfg beside the real clang and points clang++/clang-cpp/<versioned>.cfg
// at it — the envoy pattern (docs/from-scratch-toolchain.md).
func pkgxLibcBlock(o WrapOptions) string {
	if !o.PkgxLibc || o.Target.Platform != "linux" || o.Target.Arch != o.Host.Arch {
		return ""
	}
	return `if [ -n "$BK_PKGX_LIBC" ]; then
  _bk_glibclib=$(dirname "$(ls "$PKGX_DIR"/gnu.org/glibc/v*/lib/glibc-*/libc.so.6 2>/dev/null | sort | tail -1)")
  _bk_glibcld=$(ls "$_bk_glibclib"/ld-linux*.so.* 2>/dev/null | head -1)
  _bk_gcclib=$(dirname "$(ls "$PKGX_DIR"/gnu.org/gcc/libstdcxx/v*/lib*/libgcc_s.so.1 2>/dev/null | head -1)")
  _bk_clang=$(readlink -f "$(command -v clang)")
  _bk_bin=$(dirname "$_bk_clang")
  {
    echo "-B$_bk_glibclib"
    echo "-L$_bk_glibclib"
    echo "-Wl,--dynamic-linker=$_bk_glibcld"
    echo "-Wl,-rpath,$_bk_glibclib"
    echo "-Wl,-rpath,$_bk_gcclib"
    echo "-Wl,--disable-new-dtags"
  } > "$_bk_bin/clang.cfg"
  for _bk_a in clang++ clang-cpp "$(basename "$_bk_clang")"; do ln -sf clang.cfg "$_bk_bin/$_bk_a.cfg"; done
fi
`
}

// tmpdirLine sets TMPDIR (POSIX) or TMP/TEMP (windows host) under $HOME.
func tmpdirLine(host target.Target) string {
	if host.Platform == "windows" {
		return `export TMP="$HOME/tmp"; export TEMP="$HOME/tmp"; mkdir -p "$TMP"`
	}
	return `export TMPDIR="$HOME/tmp"; mkdir -p "$TMPDIR"`
}

// wrapFlags returns the target-keyed compiler/linker FLAGS exports. A windows
// target gets none (PEs have no rpath and mingw rejects -pie).
func wrapFlags(tgt target.Target, pkgxDir string, hasBinutils bool) []string {
	var out []string
	ldflags := ""
	switch tgt.Platform {
	case "darwin":
		ldflags = "-Wl,-rpath," + pkgxDir
	case "linux":
		if tgt.Arch == "x86-64" {
			ldflags = "-pie"
		}
	}
	if ldflags != "" {
		out = append(out, `export LDFLAGS="`+ldflags+` $LDFLAGS"`)
	}
	if tgt.Platform == "linux" && tgt.Arch == "x86-64" {
		out = append(out, `export CFLAGS="-fPIC $CFLAGS"`, `export CXXFLAGS="-fPIC $CXXFLAGS"`)
	}
	if tgt.Platform == "darwin" {
		out = append(out, "export MACOSX_DEPLOYMENT_TARGET=11.0")
		if hasBinutils {
			out = append(out, "export AR=/usr/bin/ar", "export RANLIB=/usr/bin/ranlib")
		}
	}
	return out
}
