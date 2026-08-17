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
	// LibcPkgx targets the pkgx gnu.org/glibc bottle instead of the build
	// container's system glibc (linux only): the output links its crt objects,
	// libc and dynamic linker from the bottle, so the bottle owes nothing to the
	// debian build container — the sovereign FROM-scratch end state. Opt-in
	// (default off); the caller also adds gnu.org/glibc to Deps so the bottle is
	// present in the eval. C-only for now: the pkgx llvm.org bottle ships no
	// libc++/libunwind, so C++ recipes still need the system libstdc++.
	LibcPkgx bool
	// Glibc, when set (with LibcPkgx), pins the exact pkgx gnu.org/glibc version
	// used as the sysroot — so the output links THAT glibc's symbols and runs on
	// any host whose kernel satisfies it (a chosen HPC floor, e.g. "2.27.0").
	// Empty = newest available. wrapFlags then resolves that sole installed
	// version dir, so no change is needed there.
	Glibc string
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
		// The dependency environment must be a HARD failure. `eval "$(pkgx +…)"`
		// swallows it: when pkgx errors, the substitution is empty, `eval ""`
		// succeeds, and the build carries on with NO deps — then dies much later
		// with something unrelated ("C compiler cannot create executables",
		// "cannot find -lncursesw") that costs an hour to trace back. Capture the
		// output, check the status, and say which command failed.
		b.WriteString("set -a\n")
		fmt.Fprintf(&b, "__bk_deps_env=\"$(CLICOLOR_FORCE=1 %s %s)\" || {\n", pkgx, plus)
		fmt.Fprintf(&b, "  echo \"bk: the dependency environment failed: %s %s\" >&2\n", pkgx, plus)
		b.WriteString("  exit 1\n}\n")
		b.WriteString("eval \"$__bk_deps_env\"\n")
		b.WriteString("unset __bk_deps_env\n")
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
	for _, f := range wrapFlags(o.Target, o.PkgxDir, o.HasBinutils, o.LibcPkgx) {
		b.WriteString(f + "\n")
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
	// pkgx-libc mode needs the gnu.org/glibc bottle present in the eval so its
	// headers/crt/libc/loader are on disk under $PKGX_DIR for wrapFlags to point
	// the compiler at (linux only). kernel.org/linux-headers supplies the Linux
	// kernel headers (linux/*.h, asm/*.h) glibc's own headers include — glibc's
	// recipe depends on it for exactly this reason; the bottle doesn't bundle
	// them and --sysroot cuts off the container's copy.
	if o.LibcPkgx && o.Target.Platform == "linux" {
		// gnu.org/binutils supplies ar/ranlib (the pkgx llvm.org bottle ships
		// llvm-ar, not a plain `ar`, and the sovereign mode must not fall back to
		// the container's binutils). Linking still uses lld via -fuse-ld=lld.
		// libcxx.llvm.org supplies the C++ runtime the bare llvm.org bottle omits:
		// its recipe builds libc++ + libc++abi + libunwind, so C++ recipes link
		// -stdlib=libc++ instead of the container's GNU libstdc++.
		glibcSpec := `"+gnu.org/glibc"`
		if o.Glibc != "" {
			// Pin the exact glibc version as the sysroot floor. Only this version
			// is then installed under $PKGX_DIR, so wrapFlags' version-sort picks it.
			//
			// The pin is `@<version>` with NO operator: pkgx accepts a bare numeric
			// after `@` and REJECTS `@=2.27.0` with "Error: invalid semver". That
			// error aborts the whole env eval, so nothing at all gets installed —
			// which is how a pinned build ended up with unexpanded glibc globs AND
			// the container's gcc (the eval never put the pkgx llvm bin/cc on PATH).
			glibcSpec = `"+gnu.org/glibc@` + strings.TrimPrefix(o.Glibc, "=") + `"`
		}
		parts = append(parts, glibcSpec, `"+kernel.org/linux-headers"`, `"+gnu.org/binutils"`, `"+libcxx.llvm.org"`)
	}
	return strings.Join(parts, " ")
}

// tmpdirLine sets TMPDIR (POSIX) or TMP/TEMP (windows host) under $HOME.
func tmpdirLine(host target.Target) string {
	if host.Platform == "windows" {
		return `export TMP="$HOME/tmp"; export TEMP="$HOME/tmp"; mkdir -p "$TMP"`
	}
	return `export TMPDIR="$HOME/tmp"; mkdir -p "$TMPDIR"`
}

// glibcLoader is the arch-specific ELF interpreter (PT_INTERP) filename glibc
// ships. It lives in the bottle's lib/glibc-<ver>/ dir.
func glibcLoader(arch string) string {
	if arch == "aarch64" || arch == "arm64" {
		return "ld-linux-aarch64.so.1"
	}
	return "ld-linux-x86-64.so.2"
}

// wrapFlags returns the target-keyed compiler/linker FLAGS exports. A windows
// target gets none (PEs have no rpath and mingw rejects -pie).
func wrapFlags(tgt target.Target, pkgxDir string, hasBinutils, libcPkgx bool) []string {
	var out []string
	var ld []string
	switch tgt.Platform {
	case "darwin":
		ld = append(ld, "-Wl,-rpath,"+pkgxDir)
	case "linux":
		// Both arches get an absolute -Wl,-rpath,$PKGX_DIR so the linker emits a
		// DT_RUNPATH *slot* fixup/rpath.go later rewrites $ORIGIN-relative. A
		// literal "$ORIGIN" here would be mangled by shell/make re-expansion, so
		// we pass the absolute PKGX_DIR and relocate it at fixup time.
		//
		// We deliberately do NOT add -pie. gcc already defaults to PIE for
		// executables (measured: debian gcc 14.2 `gcc main.c` → a PIE executable),
		// so an explicit -pie buys nothing there — but it ALSO lands on -shared
		// library links, where it conflicts with -shared and makes ld try to build
		// an executable → "undefined reference to `main`" (this broke every
		// shared-lib recipe on x86-64, e.g. openssl's libcrypto.so.4, so those
		// bottles never published for linux/x86-64). -fPIC in CFLAGS still gives
		// the position-independent objects both PIE executables and shared libs
		// need; PIE-ness of the final executable is the toolchain default.
		ld = append(ld, "-Wl,-rpath,"+pkgxDir)
	}

	// pkgx-libc mode (linux): resolve the gnu.org/glibc bottle in the eval'd
	// $PKGX_DIR at build time (its version isn't known here, so glob + version-
	// sort in the shell), then point the compiler driver at its headers, crt
	// objects, libc and dynamic linker. --rtlib=compiler-rt keeps the runtime on
	// llvm's builtins (the pkgx llvm.org bottle ships compiler-rt but no libgcc);
	// --unwindlib=none avoids the libgcc_s unwinder the C toolchain would
	// otherwise pull (fine for exception-free C — the mode's scope). The result
	// links its interpreter + libc from the bottle, not the debian container.
	var glibcCC, glibcCXX, glibcLD string
	if libcPkgx && tgt.Platform == "linux" {
		// Resolve the newest installed version dir of each bottle. Two hazards:
		// pkgx creates a symlink literally NAMED "v*" (alongside v2, v2.44,
		// v2.44.0) which a "v*/" glob would match and — worse — sort -V ranks
		// LAST, so the chosen value would still contain a wildcard that re-globs
		// into every version dir when the unquoted CFLAGS reach the linker. The
		// "v[0-9]*/" pattern skips that symlink and lands on a concrete version
		// dir (no metacharacters, glob-safe downstream). The `set +f` re-enables
		// globbing in each subshell in case pkgx's env eval left the shell noglob.
		//
		// The glob must NOT carry the trailing slash: `sort -V` compares "v2/"
		// ABOVE "v2.27.0/" (the '/' outranks '.'), so a trailing-slash pattern
		// selects the LEAST specific entry — `v2`, a FLOATING symlink. A binary
		// whose PT_INTERP goes through `v2` silently loads whatever glibc `v2`
		// points at later, which defeats the whole point of pinning a floor.
		// Globbing without the slash sorts v2 < v2.27 < v2.27.0, so the concrete
		// version dir wins; the slash is appended afterwards.
		// bkresolve prints the newest concrete version dir of a bottle, or NOTHING
		// when that bottle is not installed. A shell leaves an unmatched pattern
		// LITERAL, and a literal `v[0-9]*` in the flags poisons the link — that is
		// how a build with no libcxx bottle ended up passing
		// `-L/pkgx/libcxx.llvm.org/v[0-9]*/lib` to the linker and failing
		// configure's very first "can the compiler create executables" probe.
		out = append(out,
			`bkresolve() { set +f; for d in $1; do case "$d" in *'*'*|*'['*) continue;; esac; printf '%s\n' "$d"; done | sort -V | tail -n1; }`,
			`export BK_GLIBC_PREFIX="$(bkresolve "$PKGX_DIR/gnu.org/glibc/v[0-9]*")"`,
			`[ -n "$BK_GLIBC_PREFIX" ] && export BK_GLIBC_PREFIX="$BK_GLIBC_PREFIX/"`,
			`export BK_GLIBC_LIB="$(bkresolve "${BK_GLIBC_PREFIX}lib/glibc-[0-9]*")"`,
			`[ -n "$BK_GLIBC_LIB" ] && export BK_GLIBC_LIB="$BK_GLIBC_LIB/"`,
			`export BK_KHDR_PREFIX="$(bkresolve "$PKGX_DIR/kernel.org/linux-headers/v[0-9]*")"`,
			`[ -n "$BK_KHDR_PREFIX" ] && export BK_KHDR_PREFIX="$BK_KHDR_PREFIX/"`,
			`export BK_LIBCXX_PREFIX="$(bkresolve "$PKGX_DIR/libcxx.llvm.org/v[0-9]*")"`,
			`[ -n "$BK_LIBCXX_PREFIX" ] && export BK_LIBCXX_PREFIX="$BK_LIBCXX_PREFIX/"`,
		)
		// Shared driver flags: glibc sysroot (its headers + the kernel headers it
		// includes), glibc crt/libc, and llvm's compiler-rt builtins (no libgcc).
		// -L takes NO space: libtool parses the flag itself and rejects a
		// separated form outright ("require no space between '-L' and '/…'"),
		// which is what broke pcre2. -Wno-unused-command-line-argument keeps the
		// LINK flags (--rtlib/--unwindlib/-fuse-ld) from making clang warn when
		// it is merely compiling: a warning there turns into an error under the
		// -Werror probe several configure scripts run, and xz then refuses to
		// configure at all ("CFLAGS contains something that makes -Werror
		// complain").
		base := `--sysroot="$BK_GLIBC_PREFIX" -isystem "${BK_GLIBC_PREFIX}include" -isystem "${BK_KHDR_PREFIX}include" -B "$BK_GLIBC_LIB" -L"$BK_GLIBC_LIB" --rtlib=compiler-rt -fuse-ld=lld -Wno-unused-command-line-argument`
		// C: no unwinder (exception-free C). C++: libc++ headers + its libunwind,
		// from the libcxx.llvm.org bottle (-stdlib=libc++ makes the driver link
		// -lc++ itself when it links a C++ target).
		glibcCC = base + ` --unwindlib=none`
		// libc++'s headers MUST precede glibc's on the search path — its <cstdio>
		// pulls libc++'s own <stdio.h> wrapper and #errors if a C <stdio.h> is
		// found first — so the libc++ -isystem goes BEFORE base's glibc/kernel ones.
		// ${VAR:+…} so an ABSENT libcxx bottle contributes nothing at all rather
		// than an unmatched pattern.
		glibcCXX = `${BK_LIBCXX_PREFIX:+-stdlib=libc++ -isystem "${BK_LIBCXX_PREFIX}include/c++/v1"} ` + base + ` ${BK_LIBCXX_PREFIX:+--unwindlib=libunwind}`
		// PT_INTERP = the bottle's loader; search paths for glibc + libc++/libunwind
		// libs (the $ORIGIN rewrite by fixup drops these, so at deploy libc.so.6 /
		// libc++.so.1 come from their bottles via LD_LIBRARY_PATH — the mkscratch
		// closure model). --disable-new-dtags emits DT_RPATH (searched for the
		// loader's own NEEDED libs).
		glibcLD = `-Wl,--dynamic-linker="${BK_GLIBC_LIB}` + glibcLoader(tgt.Arch) +
			`" -Wl,-rpath,"$BK_GLIBC_LIB" ${BK_LIBCXX_PREFIX:+-L"${BK_LIBCXX_PREFIX}lib" -Wl,-rpath,"${BK_LIBCXX_PREFIX}lib"} -Wl,--disable-new-dtags`
		ld = append(ld, glibcLD)
		// Pin the compiler to the pkgx llvm one AND carry the whole driver
		// configuration inside it.
		//
		// Two reasons, both measured. (a) The flag set is clang's
		// (--rtlib/--unwindlib/-fuse-ld=lld) while the bottle provides `cc` and
		// `clang` but NOT `gcc`, and autoconf probes `gcc` FIRST — so a
		// ./configure recipe silently picked the container's gcc and died with
		// `cc: error: unrecognized command-line option '--unwindlib=none'`.
		// (b) libtool builds its own compile and LINK command lines and does not
		// pass the recipe's CFLAGS/LDFLAGS to them, so a libtool link fell back
		// to the GCC runtime — `ld.lld: cannot open crti.o … unable to find
		// -lgcc`. It does preserve $CC verbatim, so the sysroot, the crt/lib
		// search paths and the runtime choice belong there. (This is what
		// distributions do for cross toolchains.)
		//
		// Scoped to this mode: a global CC=clang was measured and rejected for
		// ordinary builds, where the flags stay compiler-neutral.
		out = append(out,
			`export CC="${CC:-clang `+glibcCC+`}"`,
			`export CXX="${CXX:-clang++ `+glibcCXX+`}"`,
			// What the cc/gcc/c++/g++ shims re-exec. A recipe that calls the
			// compiler by name instead of through $CC (sqlite's autosetup: "No
			// working C compiler found. Tried cc and gcc") then still gets the
			// sovereign driver.
			`export BK_CC="clang `+glibcCC+`"`,
			`export BK_CXX="clang++ `+glibcCXX+`"`,
		)
	}

	if len(ld) > 0 {
		out = append(out, `export LDFLAGS="`+strings.Join(ld, " ")+` $LDFLAGS"`)
	}
	if tgt.Platform == "linux" {
		// Relax the C23 hard errors that gcc-14 / clang-16 turned on by default
		// (implicit function/int declarations, int<->pointer conversions). Many
		// long-standing C recipes still trip these and upstream has not adapted;
		// the whole distro ecosystem (Fedora, Debian, …) demotes them back to
		// warnings for the mass rebuild, so the toolchain default doesn't gate an
		// otherwise-buildable recipe. C-only — CXXFLAGS keeps just the PIC flag.
		// -Wno-error= (not -Wno-) for the function-pointer one: an incompatible
		// function pointer is a real type error that can crash at run time, unlike
		// an implicit declaration, so the diagnostic must stay VISIBLE in the build
		// log even though it no longer fails the build. gnu.org/gettext trips it
		// (iconv-ostream.c:297, an ostream vtable initialiser).
		cflags := "-Wno-implicit-function-declaration -Wno-implicit-int -Wno-int-conversion -Wno-error=incompatible-function-pointer-types"
		if glibcCC != "" {
			cflags = glibcCC + " " + cflags
		}
		if tgt.Arch == "x86-64" {
			cflags = "-fPIC " + cflags
			cxxflags := "-fPIC"
			if glibcCXX != "" {
				cxxflags = glibcCXX + " " + cxxflags
			}
			out = append(out, `export CXXFLAGS="`+cxxflags+` $CXXFLAGS"`)
		} else if glibcCXX != "" {
			out = append(out, `export CXXFLAGS="`+glibcCXX+` $CXXFLAGS"`)
		}
		out = append(out, `export CFLAGS="`+cflags+` $CFLAGS"`)
	}
	if tgt.Platform == "darwin" {
		out = append(out, "export MACOSX_DEPLOYMENT_TARGET=11.0")
		if hasBinutils {
			out = append(out, "export AR=/usr/bin/ar", "export RANLIB=/usr/bin/ranlib")
		}
	}
	return out
}
