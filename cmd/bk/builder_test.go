package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/bottlepkg"
	"github.com/go-pkgx/bottle"
)

// fakeStage replaces the four bottle seams with an in-memory registry that
// materialises a plausible glibc/bash/tool layout under pkgxDir.
func fakeStage(t *testing.T) *stageCalls {
	t.Helper()
	calls := &stageCalls{}
	oldR, oldI, oldL, oldS := resolveClosureFor, installFor, findLoaderFor, stubBinsStaged
	t.Cleanup(func() {
		resolveClosureFor, installFor, findLoaderFor, stubBinsStaged = oldR, oldI, oldL, oldS
	})

	closure := []bottle.Resolved{
		{Project: bottle.GlibcProject, Version: bottle.ParseVer("2.44.0")},
		{Project: "gnu.org/bash", Version: bottle.ParseVer("5.3")},
	}
	resolveClosureFor = func(roots map[string]string, osn, arch string) ([]bottle.Resolved, error) {
		calls.resolvedFor = osn + "/" + arch
		calls.roots = roots
		return closure, nil
	}
	installFor = func(r bottle.Resolved, pkgxDir, osn, arch string) (bool, error) {
		calls.installed = append(calls.installed, r.Project)
		calls.installedFor = osn + "/" + arch
		// Lay the files down so the later steps have something real to find.
		libDir := filepath.Join(pkgxDir, r.Project, "v"+r.Version.Raw, "lib", "glibc-2.44")
		binDir := filepath.Join(pkgxDir, r.Project, "v"+r.Version.Raw, "bin")
		for _, d := range []string{libDir, binDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return false, err
			}
		}
		if r.Project == "gnu.org/bash" {
			if err := os.WriteFile(filepath.Join(binDir, "bash"), []byte("x"), 0o755); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	findLoaderFor = func(pkgxDir, arch string) string {
		calls.loaderArch = arch
		return filepath.Join(pkgxDir, bottle.GlibcProject, "v2.44.0", "lib", "glibc-2.44", bottle.LoaderNameFor(arch))
	}
	stubBinsStaged = func(clo []bottle.Resolved, s bottle.Stage) (int, error) {
		calls.stage = s
		return len(clo), nil
	}
	return calls
}

type stageCalls struct {
	resolvedFor  string
	installedFor string
	loaderArch   string
	roots        map[string]string
	installed    []string
	stage        bottle.Stage
}

func stageOpts(root string) stageOptions {
	return stageOptions{
		Root: root, OS: "linux", Arch: "aarch64",
		Roots: map[string]string{"gnu.org/glibc": "*"},
		Log:   func(string) {},
	}
}

// TestStageBuilderTargetsTheRequestedPlatform: every step must be told the
// TARGET's slug. Staging a linux/aarch64 rootfs from a darwin host through the
// host's slug is the failure this whole path exists to avoid.
func TestStageBuilderTargetsTheRequestedPlatform(t *testing.T) {
	calls := fakeStage(t)
	root := t.TempDir()

	if err := stageBuilder(stageOpts(root)); err != nil {
		t.Fatal(err)
	}

	if calls.resolvedFor != "linux/aarch64" || calls.installedFor != "linux/aarch64" {
		t.Errorf("resolved for %q, installed for %q; want linux/aarch64 both", calls.resolvedFor, calls.installedFor)
	}
	if calls.loaderArch != "aarch64" {
		t.Errorf("looked for the %q loader", calls.loaderArch)
	}
	if calls.stage.OS != "linux" || calls.stage.Arch != "aarch64" {
		t.Errorf("stubs staged for %s/%s", calls.stage.OS, calls.stage.Arch)
	}
}

// TestStageBuilderPosesGuestPaths: the symlinks a scratch rootfs lives by must
// point where the GUEST will look, never into the staging directory.
func TestStageBuilderPosesGuestPaths(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()

	if err := stageBuilder(stageOpts(root)); err != nil {
		t.Fatal(err)
	}

	loader, err := os.Readlink(filepath.Join(root, "lib", "ld-linux-aarch64.so.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loader, guestPkgxDir+"/") {
		t.Errorf("/lib loader -> %q, want a %s path", loader, guestPkgxDir)
	}
	if strings.Contains(loader, root) {
		t.Errorf("/lib loader leaks the staging root: %q", loader)
	}
	sh, err := os.Readlink(filepath.Join(root, "bin", "sh"))
	if err != nil {
		t.Fatal(err)
	}
	if sh != guestPkgxDir+"/gnu.org/bash/v5.3/bin/bash" {
		t.Errorf("/bin/sh -> %q", sh)
	}
}

// TestStageBuilderStubsIntoUsr, not /usr/local: bk builds under a sanitised
// PATH that lists /usr/bin and /bin only, so a stub anywhere else is a stub the
// build cannot call.
func TestStageBuilderStubsIntoUsr(t *testing.T) {
	calls := fakeStage(t)
	root := t.TempDir()

	if err := stageBuilder(stageOpts(root)); err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(root, "usr"); calls.stage.Prefix != want {
		t.Errorf("stub prefix = %q, want %q", calls.stage.Prefix, want)
	}
	if calls.stage.GuestDir != guestPkgxDir {
		t.Errorf("stubs bake %q, want %q", calls.stage.GuestDir, guestPkgxDir)
	}
	if want := filepath.Join(root, "pkgx"); calls.stage.Dir != want {
		t.Errorf("stubs read from %q, want %q", calls.stage.Dir, want)
	}
}

// TestStageBuilderMakesWritableDirs: a build writes. /tmp missing is not a
// subtle failure, but it is a late one — the first recipe that calls mktemp.
func TestStageBuilderMakesWritableDirs(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()

	if err := stageBuilder(stageOpts(root)); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"tmp", "var/tmp"} {
		st, err := os.Stat(filepath.Join(root, d))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if st.Mode().Perm()&0o777 != 0o777 || st.Mode()&os.ModeSticky == 0 {
			t.Errorf("%s has mode %v, want 1777", d, st.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "root")); err != nil {
		t.Errorf("no /root: %v", err)
	}
}

// TestStageBuilderInstallsBinaries checks the two drivers land where the
// entrypoint expects them, executable.
func TestStageBuilderInstallsBinaries(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "bk")
	if err := os.WriteFile(src, []byte("ELF-ish"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := stageOpts(root)
	o.BkBin = src

	if err := stageBuilder(o); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(root, "usr", "local", "bin", "bk")
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable: %v", dst, st.Mode())
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "ELF-ish" {
		t.Errorf("copied %q", b)
	}
}

// TestStageBuilderErrors covers each step's failure surfacing with its own
// message, rather than a bare "no such file" three layers down.
func TestStageBuilderErrors(t *testing.T) {
	t.Run("resolve", func(t *testing.T) {
		fakeStage(t)
		resolveClosureFor = func(map[string]string, string, string) ([]bottle.Resolved, error) {
			return nil, errors.New("boom")
		}
		if err := stageBuilder(stageOpts(t.TempDir())); err == nil || !strings.Contains(err.Error(), "resolve closure") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("install", func(t *testing.T) {
		fakeStage(t)
		installFor = func(bottle.Resolved, string, string, string) (bool, error) {
			return false, errors.New("boom")
		}
		if err := stageBuilder(stageOpts(t.TempDir())); err == nil || !strings.Contains(err.Error(), "install ") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no loader", func(t *testing.T) {
		fakeStage(t)
		findLoaderFor = func(string, string) string { return "" }
		err := stageBuilder(stageOpts(t.TempDir()))
		if err == nil || !strings.Contains(err.Error(), "loader") {
			t.Fatalf("got %v", err)
		}
		// The message must say what to DO about it.
		if !strings.Contains(err.Error(), "gnu.org/glibc") {
			t.Errorf("message does not name the missing package: %v", err)
		}
	})
	t.Run("stubs", func(t *testing.T) {
		fakeStage(t)
		stubBinsStaged = func([]bottle.Resolved, bottle.Stage) (int, error) {
			return 0, errors.New("boom")
		}
		if err := stageBuilder(stageOpts(t.TempDir())); err == nil || !strings.Contains(err.Error(), "stub binaries") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		fakeStage(t)
		o := stageOpts(t.TempDir())
		o.BkBin = filepath.Join(t.TempDir(), "absent")
		if err := stageBuilder(o); err == nil || !strings.Contains(err.Error(), "install bk") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unwritable rootfs", func(t *testing.T) {
		fakeStage(t)
		root := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(root, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := stageBuilder(stageOpts(root)); err == nil {
			t.Fatal("staging into a regular file must fail")
		}
	})
}

// TestMicroVMConfigIsBootable is what makes a staged directory runnable with no
// registry at all: weft's Pull treats this file as its "already materialised"
// sentinel.
func TestMicroVMConfigIsBootable(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()
	o := stageOpts(root)
	o.MicroVM = true
	o.Dist = "oci://ghcr.io/go-pkgx/packages"
	o.Overlay = "https://example.invalid/projects"

	if err := stageBuilder(o); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(root, ".weft-microvm", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg microVMConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Process.Args) != 1 || cfg.Process.Args[0] != "/usr/local/bin/bk" {
		t.Errorf("args = %v", cfg.Process.Args)
	}
	env := strings.Join(cfg.Process.Env, "\n")
	// The guest's init execs these args VERBATIM — no pkgx wrapper composes
	// the environment the way a container ENTRYPOINT did. Without an explicit
	// LD_LIBRARY_PATH the first thing that happens in the guest is /bin/sh
	// failing to find libc.so.6.
	if !strings.Contains(env, "LD_LIBRARY_PATH="+guestPkgxDir+"/") {
		t.Errorf("no guest LD_LIBRARY_PATH:\n%s", env)
	}
	if strings.Contains(env, root) {
		t.Errorf("env leaks the staging root:\n%s", env)
	}
	for _, want := range []string{"PKGX_DIR=" + guestPkgxDir, "PKGX_DIST=oci://", "PKGX_PANTRY_OVERLAY=https://", "HOME=/root", "TMPDIR=/tmp"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q:\n%s", want, env)
		}
	}
	if cfg.Process.Cwd != "/" {
		t.Errorf("cwd = %q", cfg.Process.Cwd)
	}
}

// TestMicroVMConfigOmitsUnsetSources: an empty --dist must not bake an empty
// PKGX_DIST, which pkgx would read as "no registry".
func TestMicroVMConfigOmitsUnsetSources(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()
	o := stageOpts(root)
	o.MicroVM = true

	if err := stageBuilder(o); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(filepath.Join(root, ".weft-microvm", "config.json"))
	if strings.Contains(string(b), "PKGX_DIST=\"") || strings.Contains(string(b), "PKGX_DIST=\n") {
		t.Errorf("baked an empty PKGX_DIST:\n%s", b)
	}
	if strings.Contains(string(b), "PKGX_PANTRY_OVERLAY") {
		t.Errorf("baked an unset overlay:\n%s", b)
	}
}

// TestMicroVMConfigUnwritable surfaces the write failure with its own message.
func TestMicroVMConfigUnwritable(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()
	// A FILE where .weft-microvm must be a directory.
	if err := os.WriteFile(filepath.Join(root, ".weft-microvm"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	o := stageOpts(root)
	o.MicroVM = true

	if err := stageBuilder(o); err == nil || !strings.Contains(err.Error(), ".weft-microvm/config.json") {
		t.Fatalf("got %v", err)
	}
}

func TestToolchainRootsDefault(t *testing.T) {
	roots, err := toolchainRoots("")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"llvm.org", "gnu.org/glibc", "gnu.org/bash", "kernel.org/linux-headers"} {
		if _, ok := roots[want]; !ok {
			t.Errorf("the built-in toolchain has no %s", want)
		}
	}
}

func TestToolchainRootsFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "toolchain.txt")
	body := "# a comment\n\n" +
		"llvm.org\n" +
		"gnu.org/glibc@^2.40   # trailing comment\n" +
		"perl.org~5.42\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	roots, err := toolchainRoots(p)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"llvm.org": "*", "gnu.org/glibc": "^2.40", "perl.org": "~5.42"}
	if len(roots) != len(want) {
		t.Fatalf("roots = %v", roots)
	}
	for k, v := range want {
		if roots[k] != v {
			t.Errorf("%s = %q, want %q", k, roots[k], v)
		}
	}
}

func TestToolchainRootsErrors(t *testing.T) {
	if _, err := toolchainRoots(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing toolchain file must fail")
	}
	p := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(p, []byte("# only comments\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := toolchainRoots(p); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty list must fail, got %v", err)
	}
}

func TestSplitToolchainSpec(t *testing.T) {
	for _, tc := range []struct{ in, proj, constraint string }{
		{"llvm.org", "llvm.org", "*"},
		{"gnu.org/glibc@^2.40", "gnu.org/glibc", "^2.40"},
		{"perl.org~5.42", "perl.org", "~5.42"},
		{"llvm.org<19", "llvm.org", "<19"},
		{"x.org=1.2.3", "x.org", "=1.2.3"},
		{"x.org@", "x.org", "*"}, // an @ with nothing after it pins nothing
	} {
		p, c := splitToolchainSpec(tc.in)
		if p != tc.proj || c != tc.constraint {
			t.Errorf("%q -> (%q,%q), want (%q,%q)", tc.in, p, c, tc.proj, tc.constraint)
		}
	}
}

func TestGuestPath(t *testing.T) {
	dir := "/stage/pkgx"
	if got := guestPath("/stage/pkgx/gnu.org/bash/v5.3/bin/bash", dir); got != "/pkgx/gnu.org/bash/v5.3/bin/bash" {
		t.Errorf("got %q", got)
	}
	if got := guestPath("", dir); got != "" {
		t.Errorf("an absent optional became %q", got)
	}
	// A path outside the staging dir is not ours to rewrite.
	if got := guestPath("/elsewhere/bin/bash", dir); got != "/elsewhere/bin/bash" {
		t.Errorf("rewrote a foreign path to %q", got)
	}
}

func TestRunBuilderUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runBuilder(nil, &out, &errb); code != 2 {
		t.Errorf("no --out: exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "--out") {
		t.Errorf("usage does not mention --out: %q", errb.String())
	}
	errb.Reset()
	if code := runBuilder([]string{"--out", t.TempDir(), "--platform", "darwin/aarch64"}, &out, &errb); code != 2 {
		t.Errorf("darwin: exit %d, want 2", code)
	}
	errb.Reset()
	if code := runBuilder([]string{"--nonsuch"}, &out, &errb); code != 2 {
		t.Errorf("bad flag: exit %d, want 2", code)
	}
}

func TestRunBuilderStages(t *testing.T) {
	calls := fakeStage(t)
	root := filepath.Join(t.TempDir(), "rootfs")
	tc := filepath.Join(t.TempDir(), "toolchain.txt")
	if err := os.WriteFile(tc, []byte("gnu.org/glibc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer

	code := runBuilder([]string{
		"--out", root, "--platform", "linux/x86-64", "--toolchain", tc,
		"--dist", "oci://example.invalid/pkgs", "--overlay", "https://example.invalid/p",
	}, &out, &errb)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if calls.resolvedFor != "linux/x86-64" {
		t.Errorf("resolved for %q", calls.resolvedFor)
	}
	if _, ok := calls.roots["gnu.org/glibc"]; !ok || len(calls.roots) != 1 {
		t.Errorf("roots = %v, want only the file's entry", calls.roots)
	}
	if bottle.DistBase != "oci://example.invalid/pkgs" {
		t.Errorf("--dist not applied: %q", bottle.DistBase)
	}
	if !strings.Contains(out.String(), "stubs in /usr/bin") {
		t.Errorf("no progress reported: %q", out.String())
	}
}

func TestRunBuilderReportsStagingFailure(t *testing.T) {
	fakeStage(t)
	resolveClosureFor = func(map[string]string, string, string) ([]bottle.Resolved, error) {
		return nil, errors.New("registry down")
	}
	var out, errb bytes.Buffer

	if code := runBuilder([]string{"--out", t.TempDir()}, &out, &errb); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "registry down") {
		t.Errorf("the cause was swallowed: %q", errb.String())
	}
}

func TestRunBuilderRejectsMissingToolchainFile(t *testing.T) {
	fakeStage(t)
	var out, errb bytes.Buffer
	code := runBuilder([]string{"--out", t.TempDir(), "--toolchain", filepath.Join(t.TempDir(), "absent")}, &out, &errb)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestCopyExecutableErrors(t *testing.T) {
	t.Run("missing source", func(t *testing.T) {
		if err := copyExecutable(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "x")); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("destination dir is a file", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "blocked")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(root, "src")
		if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := copyExecutable(src, filepath.Join(blocker, "sub", "bk")); err == nil {
			t.Fatal("want a mkdir error")
		}
	})
	t.Run("destination unwritable", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		if err := copyExecutable(src, filepath.Join(dir, "bk")); err == nil {
			t.Fatal("want an open error")
		}
	})
	t.Run("copy fails", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := ioCopy
		ioCopy = func(io.Writer, io.Reader) (int64, error) { return 0, errors.New("boom") }
		t.Cleanup(func() { ioCopy = old })
		dst := filepath.Join(root, "bk")
		if err := copyExecutable(src, dst); err == nil {
			t.Fatal("want a copy error")
		}
		// A half-written binary must not survive as if it were whole.
		if _, err := os.Stat(dst + ".tmp"); err == nil {
			t.Error("the partial file was left behind")
		}
	})
	t.Run("close fails", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := closeWritten
		closeWritten = func(*os.File) error { return errors.New("boom") }
		t.Cleanup(func() { closeWritten = old })
		dst := filepath.Join(root, "bk")
		if err := copyExecutable(src, dst); err == nil {
			t.Fatal("want a close error")
		}
		if _, err := os.Stat(dst + ".tmp"); err == nil {
			t.Error("the partial file was left behind")
		}
	})
}

func TestMicroVMConfigMarshalFailure(t *testing.T) {
	fakeStage(t)
	old := jsonMarshalIndent
	jsonMarshalIndent = func(any, string, string) ([]byte, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { jsonMarshalIndent = old })
	o := stageOpts(t.TempDir())
	o.MicroVM = true

	if err := stageBuilder(o); err == nil || !strings.Contains(err.Error(), ".weft-microvm/config.json") {
		t.Fatalf("got %v", err)
	}
}

// TestStageBuilderReportsUnwritableDirs: /tmp and friends are created after
// the closure is on disk, so their failure needs its own reachable case.
func TestStageBuilderReportsUnwritableDirs(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()
	// A FILE named tmp: MkdirAll fails at the very first writable dir.
	if err := os.WriteFile(filepath.Join(root, "tmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := stageBuilder(stageOpts(root)); err == nil {
		t.Fatal("want an error when /tmp cannot be created")
	}
}

// TestStageBuilderReportsChmodFailure covers the sticky-bit step on its own.
// Through a seam: a symlink-to-nowhere makes MkdirAll fail FIRST, so the test
// that looked like it covered this actually covered the line above it.
func TestStageBuilderReportsChmodFailure(t *testing.T) {
	fakeStage(t)
	old := osChmod
	osChmod = func(string, os.FileMode) error { return errors.New("boom") }
	t.Cleanup(func() { osChmod = old })

	if err := stageBuilder(stageOpts(t.TempDir())); err == nil {
		t.Fatal("want an error when /tmp cannot be chmod'd")
	}
}

// TestStageBuilderPoseFailure: the loader symlinks land in /lib and /lib64.
func TestStageBuilderPoseFailure(t *testing.T) {
	fakeStage(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lib"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := stageBuilder(stageOpts(root)); err == nil || !strings.Contains(err.Error(), "pose loader") {
		t.Fatalf("got %v", err)
	}
}

// TestStageBuilderSkipsAlreadyInstalled keeps the "already present" path
// honest: a re-stage into a warm directory must not claim to install again.
func TestStageBuilderSkipsAlreadyInstalled(t *testing.T) {
	fakeStage(t)
	old := installFor
	installFor = func(r bottle.Resolved, pkgxDir, osn, arch string) (bool, error) {
		if _, err := old(r, pkgxDir, osn, arch); err != nil {
			return false, err
		}
		return false, nil // already there
	}
	root := t.TempDir()
	var logged []string

	o := stageOpts(root)
	o.Log = func(s string) { logged = append(logged, s) }
	if err := stageBuilder(o); err != nil {
		t.Fatal(err)
	}

	for _, l := range logged {
		if strings.HasPrefix(l, "  + ") {
			t.Errorf("reported an install that did not happen: %q", l)
		}
	}
}

// TestBuilderIsDispatched: a subcommand nobody routes to is a subcommand that
// does not exist.
func TestBuilderIsDispatched(t *testing.T) {
	code, _, errb := run2(t, "builder")

	if code != 2 {
		t.Fatalf("bk builder: exit %d, want the usage exit 2", code)
	}
	if !strings.Contains(errb, "--out") {
		t.Errorf("dispatched somewhere else: %q", errb)
	}
}

func TestSetCodec(t *testing.T) {
	orig := bottlepkg.Codec
	t.Cleanup(func() { bottlepkg.Codec = orig })

	for _, tc := range []struct{ in, want string }{
		{"gzip", bottle.ExtTarGz},
		{"gz", bottle.ExtTarGz},
		{"zstd", bottle.ExtTarZst},
		{"zst", bottle.ExtTarZst},
	} {
		if err := setCodec(tc.in); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if bottlepkg.Codec != tc.want {
			t.Errorf("--compress %s → %q, want %q", tc.in, bottlepkg.Codec, tc.want)
		}
	}
	if err := setCodec("brotli"); err == nil {
		t.Error("an unknown codec must be refused, not silently ignored")
	}
}

// TestFactoryRejectsUnknownCompress: the flag is validated before anything is
// built, so a typo costs a message rather than a run.
func TestFactoryRejectsUnknownCompress(t *testing.T) {
	orig := bottlepkg.Codec
	t.Cleanup(func() { bottlepkg.Codec = orig })
	var out, errb bytes.Buffer

	if code := runFactory([]string{"--compress", "brotli", "--platform", "linux/aarch64", "--recipes", "lz4.org", "--pantry", t.TempDir()}, &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "brotli") {
		t.Errorf("the message does not name the bad value: %q", errb.String())
	}
}
