package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	if name == "c++" || name == "g++" {
		varName = "BK_CXX"
	}
	driver := strings.Fields(os.Getenv(varName))
	if len(driver) == 0 {
		fmt.Fprintf(stderr, "bk %s: %s is not set (this shim only belongs in a --libc=pkgx build)\n", name, varName)
		return 127
	}
	cmd := execCommand(driver[0], append(driver[1:], args...)...)
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
	for _, base := range []string{"cc", "gcc", "c++", "g++"} {
		if name == base || strings.HasSuffix(name, "-"+base) {
			return true
		}
	}
	return false
}
