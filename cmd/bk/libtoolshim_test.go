package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeScript makes an executable stand-in that reports which one ran.
func writeScript(t *testing.T, dir, name, says string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+says+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The two invocations that actually broke, both from this factory's own runs:
// x265's recipe line and the one V8's gyp generates inside nodejs. GNU libtool
// answers `unrecognised option` to each, after the build has compiled
// everything.
func TestLibtoolShimRoutesAppleInvocationsToApple(t *testing.T) {
	dir := t.TempDir()
	apple := writeScript(t, filepath.Join(dir, "usr"), "libtool", "APPLE")
	gnuDir := filepath.Join(dir, "pkgx")
	writeScript(t, gnuDir, "libtool", "GNU")

	old := appleLibtool
	appleLibtool = apple
	t.Cleanup(func() { appleLibtool = old })
	t.Setenv("PATH", gnuDir)

	for _, args := range [][]string{
		{"-static", "-o", "libx265.a", "libx265_main.a"},
		{"-framework", "CoreFoundation", "-static", "-o", "libv8_libbase.a"},
		{"-dynamic", "-o", "libfoo.dylib"},
	} {
		var out bytes.Buffer
		if code := runLibtoolCapturing(t, args, &out); code != 0 {
			t.Fatalf("args %v: exit %d (%s)", args, code, out.String())
		}
		if got := strings.TrimSpace(out.String()); got != "APPLE" {
			t.Errorf("args %v ran %q, want APPLE", args, got)
		}
	}
}

// Everything else falls through to whatever PATH would have found. The rule is
// one-directional on purpose: the shim can turn an error into a build, and
// must not be able to send a working GNU call somewhere else.
func TestLibtoolShimLeavesGnuInvocationsAlone(t *testing.T) {
	dir := t.TempDir()
	apple := writeScript(t, filepath.Join(dir, "usr"), "libtool", "APPLE")
	gnuDir := filepath.Join(dir, "pkgx")
	writeScript(t, gnuDir, "libtool", "GNU")

	old := appleLibtool
	appleLibtool = apple
	t.Cleanup(func() { appleLibtool = old })
	t.Setenv("PATH", gnuDir)

	for _, args := range [][]string{
		{"--mode=link", "cc", "-o", "libfoo.la"},
		{"--version"},
		{"--mode=compile", "cc", "-c", "foo.c"},
	} {
		var out bytes.Buffer
		if code := runLibtoolCapturing(t, args, &out); code != 0 {
			t.Fatalf("args %v: exit %d (%s)", args, code, out.String())
		}
		if got := strings.TrimSpace(out.String()); got != "GNU" {
			t.Errorf("args %v ran %q, want GNU", args, got)
		}
	}
}

// With no other libtool on PATH the shim must say so rather than recurse into
// itself: every shim in the libexec dir is a symlink to the bk binary, so a
// self-exec would loop.
func TestLibtoolShimRefusesToFindOnlyItself(t *testing.T) {
	t.Setenv("PATH", filepath.Dir(mustExecutable(t)))
	var out bytes.Buffer
	if code := libtoolShim([]string{"--version"}, &out); code != 127 {
		t.Errorf("exit = %d, want 127", code)
	}
	if !strings.Contains(out.String(), "besides this shim") {
		t.Errorf("stderr = %q, want it to name the problem", out.String())
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// runLibtoolCapturing runs the shim with stdout redirected, since the shim
// hands the child its own stdout.
func runLibtoolCapturing(t *testing.T, args []string, out *bytes.Buffer) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := libtoolShim(args, out)
	os.Stdout = old
	w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	out.Write(buf[:n])
	return code
}

// The child's exit status is the shim's: a build system reads it to decide
// whether the archive was made.
func TestLibtoolShimPassesTheExitStatusThrough(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "libtool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	if code := libtoolShim([]string{"--version"}, &out); code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
}

// A libtool on PATH that cannot be executed at all is not an exit status, and
// must be reported rather than swallowed into a zero.
func TestLibtoolShimReportsAnUnrunnableTool(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "libtool")
	// executable bit set, but not a program
	if err := os.WriteFile(p, []byte("\x00\x01not a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	if code := libtoolShim([]string{"--version"}, &out); code != 127 {
		t.Errorf("exit = %d, want 127", code)
	}
	if !strings.Contains(out.String(), "bk libtool:") {
		t.Errorf("stderr = %q, want it to name the shim", out.String())
	}
}

// Without a path to itself the shim cannot tell which PATH entry is its own,
// so it declines rather than risk exec'ing itself in a loop.
func TestLibtoolShimWithoutAPathToItself(t *testing.T) {
	old := osExecutable
	osExecutable = func() (string, error) { return "", errInjected }
	t.Cleanup(func() { osExecutable = old })
	var out bytes.Buffer
	if code := libtoolShim([]string{"--version"}, &out); code != 127 {
		t.Errorf("exit = %d, want 127", code)
	}
}

var errInjected = errors.New("injected")

// main() routes the shim name to the shim.
func TestLibtoolMultiCallDispatch(t *testing.T) {
	oldExit, oldArgs := osExit, os.Args
	defer func() { osExit, os.Args = oldExit, oldArgs }()
	got := -1
	osExit = func(c int) { got = c }
	dir := t.TempDir()
	p := filepath.Join(dir, "libtool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	os.Args = []string{"/build/libexec/libtool", "--version"}
	main()
	if got != 4 {
		t.Fatalf("libtool dispatch exit = %d, want 4", got)
	}
}
