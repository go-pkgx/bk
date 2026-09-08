package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCCShim: the shim re-execs the sovereign driver, so a recipe that calls
// `cc` or `gcc` by name gets the sysroot/crt/runtime flags $CC carries.
func TestCCShim(t *testing.T) {
	old := execCommand
	defer func() { execCommand = old }()

	var gotName string
	var gotArgs []string
	execCommand = func(name string, args ...string) *exec.Cmd {
		gotName, gotArgs = name, args
		return exec.Command("true")
	}

	t.Setenv("BK_CC", "clang --sysroot=/pkgx/glibc -fuse-ld=lld")
	t.Setenv("BK_CXX", "clang++ -stdlib=libc++")

	var errb bytes.Buffer
	if code := ccShim("cc", []string{"-c", "x.c"}, &errb); code != 0 {
		t.Fatalf("code = %d, %s", code, errb.String())
	}
	// -c compiles and never links, so the link-only flags are dropped: clang
	// warns about them there, and a caller may ask for that warning back as an
	// error (the Linux kernel passes -Werror=unused-command-line-argument).
	if gotName != "clang" || strings.Join(gotArgs, " ") != "--sysroot=/pkgx/glibc -c x.c" {
		t.Fatalf("ran %q %v", gotName, gotArgs)
	}
	// c++/g++ take the C++ driver
	for _, n := range []string{"c++", "g++"} {
		if code := ccShim(n, nil, &errb); code != 0 {
			t.Fatalf("%s: code = %d", n, code)
		}
		if gotName != "clang++" {
			t.Fatalf("%s ran %q, want the C++ driver", n, gotName)
		}
	}
	// gcc is the C driver too
	if code := ccShim("gcc", nil, &errb); code != 0 || gotName != "clang" {
		t.Fatalf("gcc ran %q (code %d)", gotName, code)
	}
}

// TestCCShimFailures: without the driver the shim says so instead of pretending
// to be a compiler, and a compiler's own exit code is passed through.
func TestCCShimFailures(t *testing.T) {
	old := execCommand
	defer func() { execCommand = old }()

	t.Setenv("BK_CC", "")
	var errb bytes.Buffer
	if code := ccShim("cc", nil, &errb); code != 127 || !strings.Contains(errb.String(), "BK_CC is not set") {
		t.Fatalf("code = %d, stderr = %q", code, errb.String())
	}

	t.Setenv("BK_CC", "clang")
	execCommand = func(string, ...string) *exec.Cmd { return exec.Command("sh", "-c", "exit 3") }
	errb.Reset()
	if code := ccShim("cc", nil, &errb); code != 3 {
		t.Fatalf("code = %d, want the compiler's own 3", code)
	}

	execCommand = func(string, ...string) *exec.Cmd { return exec.Command("/nonexistent/compiler") }
	errb.Reset()
	if code := ccShim("cc", nil, &errb); code != 127 || errb.Len() == 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb.String())
	}
}

func TestErrorsAs(t *testing.T) {
	var ee *exec.ExitError
	if errorsAs(errors.New("plain"), &ee) {
		t.Fatal("a plain error is not an ExitError")
	}
	if err := exec.Command("sh", "-c", "exit 2").Run(); !errorsAs(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("errorsAs missed the ExitError: %v", err)
	}
	_ = os.Getenv
}

// TestCCShimDispatch covers the multi-call route: bk invoked as `cc` from a
// build's libexec dir is the compiler shim.
func TestCCShimDispatch(t *testing.T) {
	oldExit, oldArgs, oldCmd := osExit, os.Args, execCommand
	defer func() { osExit, os.Args, execCommand = oldExit, oldArgs, oldCmd }()

	got := -1
	osExit = func(c int) { got = c }
	var ran string
	execCommand = func(name string, args ...string) *exec.Cmd { ran = name; return exec.Command("true") }
	t.Setenv("BK_CC", "clang --sysroot=/pkgx/glibc")

	os.Args = []string{"/build/libexec/cc", "-c", "x.c"}
	main()
	if got != 0 || ran != "clang" {
		t.Fatalf("dispatch: exit %d, ran %q", got, ran)
	}
}

// The multi-call dispatch must recognise a triple-prefixed compiler name, and
// must NOT capture something merely ending in those letters.
func TestIsCompilerShim(t *testing.T) {
	for _, yes := range []string{
		"cc", "gcc", "c++", "g++",
		"x86_64-pc-linux-gnu-gcc", "x86_64-unknown-linux-gnu-g++",
		"aarch64-unknown-linux-gnu-cc", "x86_64-pc-linux-gnu-c++",
	} {
		if !isCompilerShim(yes) {
			t.Errorf("%q must be a compiler shim", yes)
		}
	}
	for _, no := range []string{
		"bk", "fix-shebangs.ts", "python-venv.sh",
		"ccache", // ends in cc? no — but a near miss worth pinning
		"protoc", // ends in "c", not "-cc"
		"gccmakedep",
	} {
		if isCompilerShim(no) {
			t.Errorf("%q must NOT be a compiler shim", no)
		}
	}
}

// The link flags are dropped only where they mean nothing. A link invocation
// still needs every one of them — without -fuse-ld=lld and --rtlib=compiler-rt
// the sovereign toolchain links against the container's runtime instead.
func TestCCShimKeepsLinkFlagsWhenLinking(t *testing.T) {
	saved := execCommand
	defer func() { execCommand = saved }()
	var gotArgs []string
	execCommand = func(name string, args ...string) *exec.Cmd {
		gotArgs = args
		return exec.Command("true")
	}
	t.Setenv("BK_CC", "clang --sysroot=/pkgx/glibc -L/pkgx/lib --rtlib=compiler-rt -fuse-ld=lld --unwindlib=none")
	var errb bytes.Buffer
	if code := ccShim("cc", []string{"a.o", "-o", "a.out"}, &errb); code != 0 {
		t.Fatalf("code = %d, %s", code, errb.String())
	}
	for _, want := range []string{"-L/pkgx/lib", "--rtlib=compiler-rt", "-fuse-ld=lld", "--unwindlib=none"} {
		if !strings.Contains(strings.Join(gotArgs, " "), want) {
			t.Errorf("a link lost %s: %v", want, gotArgs)
		}
	}
}

// -S and -E stop before linking too.
func TestCCShimCompileOnlyForms(t *testing.T) {
	saved := execCommand
	defer func() { execCommand = saved }()
	var gotArgs []string
	execCommand = func(name string, args ...string) *exec.Cmd {
		gotArgs = args
		return exec.Command("true")
	}
	t.Setenv("BK_CC", "clang --sysroot=/pkgx/glibc -L/pkgx/lib --rtlib=compiler-rt")
	var errb bytes.Buffer
	for _, form := range []string{"-S", "-E"} {
		if code := ccShim("cc", []string{form, "x.c"}, &errb); code != 0 {
			t.Fatalf("%s: code = %d", form, code)
		}
		got := strings.Join(gotArgs, " ")
		if strings.Contains(got, "-L") || strings.Contains(got, "--rtlib") {
			t.Errorf("%s kept a link flag: %v", form, gotArgs)
		}
		if !strings.Contains(got, "--sysroot=/pkgx/glibc") {
			t.Errorf("%s lost the sysroot, which it needs: %v", form, gotArgs)
		}
	}
}
