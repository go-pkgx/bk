package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The compiler shim is where -ffile-prefix-map lives because it is the one
// layer a recipe cannot clobber: a recipe's env replaces CFLAGS/CXXFLAGS/CC
// whole, but a bare `cc`/`clang` still resolves to the shim (go-pkgx/bk#387).

// TestCCShimAppendsPrefixMapLast: the flag is the LAST argument, after whatever
// the recipe passed (including its own, which GCC and Clang resolve
// last-wins), and absent when no map is exported.
func TestCCShimAppendsPrefixMapLast(t *testing.T) {
	fakeDrivers(t)
	old := execCommand
	defer func() { execCommand = old }()
	var got []string
	execCommand = func(_ string, args ...string) *exec.Cmd {
		got = args
		return exec.Command("true")
	}
	t.Setenv("BK_CC", "clang")
	var errb bytes.Buffer

	t.Setenv(prefixMapEnv, "/Users/runner/Library/Caches/brewkit=/build")
	if code := ccShim("cc", []string{"-c", "-ffile-prefix-map=/x=/y", "a.c"}, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	want := "-ffile-prefix-map=/Users/runner/Library/Caches/brewkit=/build"
	if got[len(got)-1] != want {
		t.Fatalf("last arg = %q, want %q (args %v)", got[len(got)-1], want, got)
	}

	// control: with no map exported the flag must NOT appear, so the assertion
	// above is about the env and not about a constant.
	t.Setenv(prefixMapEnv, "")
	_ = os.Unsetenv(prefixMapEnv)
	if code := ccShim("cc", []string{"-c", "a.c"}, &errb); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(strings.Join(got, " "), "-ffile-prefix-map") {
		t.Fatalf("flag present without a map: %v", got)
	}
}

// TestCCShimToolchainRejectingFlagFailsLoudly: a driver that does not know
// -ffile-prefix-map (GCC < 8, Clang < 10) must make the build FAIL, with the
// driver's own message, and the shim must not retry without the flag.
func TestCCShimToolchainRejectingFlagFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho x >> '" + count + "'\n" +
		"for a in \"$@\"; do case \"$a\" in -ffile-prefix-map=*) echo \"cc: error: unknown argument: $a\" >&2; exit 1;; esac; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "oldcc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BK_CC", "oldcc")
	t.Setenv(prefixMapEnv, "/cache=/build")
	var errb bytes.Buffer
	code := ccShim("cc", []string{"-c", "a.c"}, &errb)
	if code == 0 {
		t.Fatal("a toolchain rejecting the flag must fail the build, got exit 0")
	}
	if !strings.Contains(errb.String(), "unknown argument") {
		t.Fatalf("the driver's own error must reach the log, got %q", errb.String())
	}
	calls, _ := os.ReadFile(count)
	if n := strings.Count(string(calls), "x"); n != 1 {
		t.Fatalf("driver ran %d times, want exactly 1 (no silent retry without the flag)", n)
	}
}

// TestCCShimRealCompilerRewritesPath drives the REAL system compiler (Clang
// >= 10 / GCC >= 8 accept the flag; this checks rather than assumes). The
// object compiled under the cache root must not contain it with the map, and
// MUST contain it without: the control that makes the zero mean something.
func TestCCShimRealCompilerRewritesPath(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no cc on PATH")
	}
	root := t.TempDir()
	src := filepath.Join(root, "p", "v1", "build", "a.c")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("const char *where(void){return __FILE__;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compile := func(mapEnv string) []byte {
		obj := filepath.Join(t.TempDir(), "a.o")
		t.Setenv("BK_CC", "")
		t.Setenv(prefixMapEnv, mapEnv)
		var errb bytes.Buffer
		// -g adds debug info, __FILE__ the macro: the two mechanisms in the issue.
		if code := ccShim("cc", []string{"-g", "-c", src, "-o", obj}, &errb); code != 0 {
			t.Fatalf("compiler failed (%d): %s", code, errb.String())
		}
		b, err := os.ReadFile(obj)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if without := compile(""); !bytes.Contains(without, []byte(root)) {
		t.Fatalf("control failed: unmapped object does not contain the root %q, so a zero below would be vacuous", root)
	}
	with := compile(root + "=/build")
	if bytes.Contains(with, []byte(root)) {
		t.Fatalf("mapped object still contains the cache root %q", root)
	}
	if !bytes.Contains(with, []byte("/build/p/v1/build/a.c")) {
		t.Fatal("mapped object lacks the rewritten /build path")
	}
}

func TestShimBase(t *testing.T) {
	for in, want := range map[string]string{
		"cc": "cc", "gcc": "gcc", "x86_64-pc-linux-gnu-gcc": "gcc",
		"s390x-ibm-linux-gnu-g++": "g++", "clang++": "clang++", "weird": "weird",
	} {
		if got := shimBase(in); got != want {
			t.Errorf("shimBase(%q) = %q, want %q", in, got, want)
		}
	}
}
