package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ccShim implements the cc/gcc/c++/g++ multi-call helpers bk materialises into
// a build's libexec dir in pkgx-libc mode.
//
// A recipe is free to call the compiler by name rather than through $CC —
// sqlite's autosetup does ("No working C compiler found. Tried cc and gcc"),
// and autoconf probes `gcc` before `cc`. A bare compiler has none of the
// sysroot, crt and runtime flags the sovereign mode depends on, so it either
// fails outright or, worse, quietly links against whatever the build container
// happens to provide. The shim re-execs the real driver WITH those flags, which
// wrapper.go exported as BK_CC / BK_CXX.
func ccShim(name string, args []string, stderr io.Writer) int {
	varName := "BK_CC"
	if isCXXShim(name) {
		varName = "BK_CXX"
	}
	driver := strings.Fields(os.Getenv(varName))
	if len(driver) == 0 {
		fmt.Fprintf(stderr, "bk %s: %s is not set (this shim only belongs in a --libc=pkgx build)\n", name, varName)
		return 127
	}
	flags := driver[1:]
	if compileOnly(args) {
		flags = withoutLinkFlags(flags)
	}
	// Resolve the driver OURSELVES, skipping the directory this shim was
	// invoked from. Without that, a shim named after the driver finds
	// itself and re-execs forever — which is why `clang` and `clang++`
	// were left unshimmed, and why the 13 recipes that call clang by name
	// got a bare compiler with no sysroot (go-pkgx/bk#302).
	bin, err := resolveDriver(driver[0], os.Args[0])
	if err != nil {
		fmt.Fprintf(stderr, "bk %s: %v\n", name, err)
		return 127
	}
	cmd := execCommand(bin, append(flags, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errorsAs(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "bk %s: %v\n", name, err)
		return 127
	}
	return 0
}

// compileOnly reports whether this invocation stops before linking: -c, -S and
// -E all produce an object, assembly or preprocessed text and never run a
// linker.
func compileOnly(args []string) bool {
	for _, a := range args {
		switch a {
		case "-c", "-S", "-E":
			return true
		}
	}
	return false
}

// withoutLinkFlags drops the driver flags that only mean something when
// linking.
//
// bk hands the same sovereign flags to every invocation, and clang warns about
// the link ones when it is merely compiling. That warning is normally harmless
// — bk also passes -Wno-unused-command-line-argument — but a caller may ask for
// it back as an error, and the Linux kernel does exactly that:
//
//	-Werror=unused-command-line-argument
//
// -Werror=X re-enables X even when -Wno-X came earlier, and bk's flags are
// placed first, so the kernel wins on position and every compile-only step
// fails. Worse, kbuild decides which warnings to disable by PROBING the
// compiler with a trivial compile: those probes fail too, so the kernel
// concludes the compiler supports none of the warnings it wanted to turn off
// and loses -Wno-address-of-packed-member and its neighbours as well. One
// collision, a cascade of unrelated-looking errors.
//
// Removing the flags where they mean nothing settles it for every caller
// instead of adding a -Wno- for each one that notices.
func withoutLinkFlags(flags []string) []string {
	out := flags[:0:0]
	for _, f := range flags {
		if strings.HasPrefix(f, "-L") || strings.HasPrefix(f, "--rtlib=") ||
			strings.HasPrefix(f, "-fuse-ld=") || strings.HasPrefix(f, "--unwindlib=") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// seams so the shim's failure branches are testable without a real compiler.
var execCommand = exec.Command

// errorsAs is errors.As, named so the shim reads without an import alias.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// isCompilerShim reports whether a name is one of the compiler shims bk
// materialises: the bare `cc`/`gcc`/`c++`/`g++`, or a triple-prefixed spelling
// of one — `x86_64-pc-linux-gnu-gcc` is what autoconf looks for first, and
// finding the bare compiler instead of the shim is how libisl's configure got
// a gcc with none of the sovereign flags and reported
//
//	configure: error: C compiler cannot create executables
//
// Matched by suffix so this stays level with buildscript's list without
// repeating every triple.
func isCompilerShim(name string) bool {
	for _, base := range compilerShimBases {
		if name == base || strings.HasSuffix(name, "-"+base) {
			return true
		}
	}
	return false
}

// isCXXShim picks the C++ driver. By SUFFIX, so a triple-prefixed name is
// routed like its bare one: `s390x-ibm-linux-gnu-g++` is a C++ compiler and
// used to fall through to BK_CC, which would have handed a C++ build the C
// driver with no libc++ headers.
func isCXXShim(name string) bool {
	for _, base := range []string{"c++", "g++", "clang++"} {
		if name == base || strings.HasSuffix(name, "-"+base) {
			return true
		}
	}
	return false
}

// compilerShimBases is the single list of names that are shims, shared with
// buildscript's materialiser so the set that is CREATED and the set that is
// DISPATCHED cannot drift apart. They did not drift, but they were two
// literals saying the same thing, which is the same bet.
var compilerShimBases = []string{"cc", "gcc", "c++", "g++", "clang", "clang++"}

// resolveDriver finds the real compiler for a shim, skipping the directory
// the shim itself lives in.
//
// # WHY A SHIM CANNOT JUST EXEC ITS DRIVER BY NAME
//
// $BK_CC is `clang <sovereign flags>`, and the shim dir is FIRST on PATH.
// So a shim named `clang` that exec'd "clang" would find itself, forever.
// That is the whole reason `clang` and `clang++` were kept out of the shim
// set — and the cost of keeping them out is measurable: of the pantry's
// recipes, 77 set CC or CXX in an `env:` block, overwhelmingly to the bare
// words `clang`, `gcc`, `clang++`, `g++`. A recipe's env is emitted AFTER
// the sovereign preamble and replaces the variable whole, so every one of
// those builds with no --sysroot, no -isystem and no crt path. gnu.org/
// autoconf and gnu.org/automake fail exactly there:
//
//	checking for gcc... clang
//	checking whether the C compiler works... no
//	configure: error: C compiler cannot create executables
//
// Skipping our own directory removes the loop, so the names can be shimmed
// and the doctrine holds: a sovereign setting that must apply to every
// recipe goes where the SHIM imposes it, not in a variable recipes declare.
//
// An absolute or relative path is taken as given — only a bare name is
// looked up, because only a bare name can resolve back to the shim.
func resolveDriver(driver, self string) (string, error) {
	if strings.ContainsRune(driver, os.PathSeparator) {
		return driver, nil
	}
	selfDir := ""
	if abs, err := filepath.Abs(filepath.Dir(self)); err == nil {
		selfDir = abs
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil && abs == selfDir {
			continue // our own shim dir
		}
		cand := filepath.Join(dir, driver)
		if fi, err := osStat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%s is not on PATH outside %s — $BK_CC names a compiler that is not installed", driver, selfDir)
}

// osStat is a seam.
var osStat = os.Stat
