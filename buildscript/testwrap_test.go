package buildscript

import (
	"strings"
	"testing"

	"github.com/go-pkgx/bk/target"
)

func linuxHost() target.Target {
	return target.Target{Platform: "linux", Arch: "x86-64", Triple: "x86_64-linux-gnu"}
}

func TestWrapTestRunsThePackageInAnEmptySandbox(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "gawk --version | grep 5.4.1",
		Package:    "gnu.org/gawk@5.4.1",
		Deps:       []string{"gnu.org/diffutils"},
		Home:       "/bk/home", Sandbox: "/bk/testbed",
		PkgxDir: "/opt/pkgx", PkgxBin: "/opt/pkgx/bin/pkgx", BashPath: "/opt/bash",
		Host: linuxHost(),
	})
	wants := []string{
		"#!/opt/bash\n",
		"set -eo pipefail",
		`export HOME="/bk/home"`,
		`export PKGX_DIR="/opt/pkgx"`,
		`export TMPDIR="$HOME/tmp"; mkdir -p "$TMPDIR"`,
		`__bk_deps_env="$(CLICOLOR_FORCE=1 /opt/pkgx/bin/pkgx "+gnu.org/gawk@5.4.1" "+gnu.org/diffutils")" || {`,
		`bk: the test environment failed`,
		`export PKGX="/opt/pkgx/bin/pkgx"`,
		"set -x",
		`cd "/bk/testbed"`,
		"gawk --version | grep 5.4.1",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

// A test compiles nothing and links nothing, so the build's flags and its
// default compiler must not be there. A test that passed because CFLAGS
// pointed at the store would be answering a question nobody asked.
func TestWrapTestHasNoneOfTheBuildsToolchain(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "true", Package: "zlib.net@1.3.2",
		Home: "/h", Sandbox: "/box", PkgxDir: "/opt/pkgx", PkgxBin: "pkgx",
		Host: linuxHost(),
	})
	for _, forbidden := range []string{
		"CFLAGS", "CXXFLAGS", "LDFLAGS", "CMAKE_PREFIX_PATH", "+llvm.org", "SRCROOT",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("test script carries the build's %s:\n%s", forbidden, s)
		}
	}
}

// The package under test leads the eval so a test dependency's closure cannot
// put another version of the same project ahead of it on PATH.
func TestWrapTestPutsThePackageUnderTestFirst(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "true", Package: "p@1", Deps: []string{"a", "b"},
		Home: "/h", Sandbox: "/box", PkgxBin: "pkgx", Host: linuxHost(),
	})
	want := `pkgx "+p@1" "+a" "+b"`
	if !strings.Contains(s, want) {
		t.Errorf("want %q in:\n%s", want, s)
	}
}

// A recipe may name no test dependency at all, and most do not. The eval then
// carries the package alone — not an empty `pkgx` with no arguments, which
// would succeed and give the test nothing.
func TestWrapTestWithNoDepsStillInstallsThePackage(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "true", Package: "p@1",
		Home: "/h", Sandbox: "/box", PkgxBin: "pkgx", Host: linuxHost(),
	})
	if !strings.Contains(s, `pkgx "+p@1"`) {
		t.Errorf("the package is not in the eval:\n%s", s)
	}
}

func TestWrapTestDefaultsBashAndPkgx(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "true", Package: "p@1", Home: "/h", Sandbox: "/box", Host: linuxHost(),
	})
	if !strings.HasPrefix(s, "#!/bin/bash\n") {
		t.Errorf("want the default shebang, got:\n%s", s)
	}
	if !strings.Contains(s, `pkgx "+p@1"`) {
		t.Errorf("want the default pkgx, got:\n%s", s)
	}
	// With no PkgxBin there is nothing to export as $PKGX; a recipe that reads
	// it would otherwise get an empty string that looks like a path.
	if strings.Contains(s, "export PKGX=") {
		t.Errorf("exported an empty PKGX:\n%s", s)
	}
	// Nor an empty PKGX_DIR, for the same reason.
	if strings.Contains(s, "export PKGX_DIR=") {
		t.Errorf("exported an empty PKGX_DIR:\n%s", s)
	}
}

// testPlus with nothing at all: `writeDepEval` must emit no eval rather than
// `pkgx ` with no arguments.
func TestWrapTestWithNothingToInstallEmitsNoEval(t *testing.T) {
	s := WrapTest(TestWrapOptions{UserScript: "true", Home: "/h", Sandbox: "/box", Host: linuxHost()})
	if strings.Contains(s, "__bk_deps_env") {
		t.Errorf("emitted an eval with nothing to install:\n%s", s)
	}
}

// 260 recipes' tests compile. A sandbox that refuses a compiler reports them
// all as failing packages.
func TestWrapTestAsksForACompilerWhenTheTestCompiles(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "cc test.c -lz", Package: "zlib.net@1.3.2", Compiler: true,
		Home: "/h", Sandbox: "/box", PkgxBin: "pkgx", Host: linuxHost(),
	})
	if !strings.Contains(s, `"+llvm.org"`) {
		t.Errorf("no compiler in the eval:\n%s", s)
	}
	// After the recipe's own, so a recipe that names a compiler keeps its
	// version.
	if i, j := strings.Index(s, `"+zlib.net@1.3.2"`), strings.Index(s, `"+llvm.org"`); i > j {
		t.Errorf("the compiler came before the package under test:\n%s", s)
	}
}

func TestWrapTestAsksForNoCompilerWhenTheTestDoesNot(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "gawk --version", Package: "gnu.org/gawk@5.4.1",
		Home: "/h", Sandbox: "/box", PkgxBin: "pkgx", Host: linuxHost(),
	})
	if strings.Contains(s, "llvm.org") {
		t.Errorf("installed a compiler for a test that compiles nothing:\n%s", s)
	}
}

// darwin builds with the system toolchain and Wrap makes the same exception;
// adding llvm.org there would give the test a compiler the build never used.
func TestWrapTestTakesDarwinsCompilerFromTheHost(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "cc test.c", Package: "p@1", Compiler: true,
		Home: "/h", Sandbox: "/box", PkgxBin: "pkgx",
		Host: target.Target{Platform: "darwin", Arch: "aarch64"},
	})
	if strings.Contains(s, "llvm.org") {
		t.Errorf("darwin must not pull llvm.org:\n%s", s)
	}
}

// The test eval exits with EnvFailExit, the build's with 1. The caller can
// then tell "I could not assemble the environment" from "the thing I ran
// said no" — five of nine failures in the s390x seed's first sweep were the
// former, and were reported as the latter.
func TestWrapTestEnvFailureHasItsOwnExitStatus(t *testing.T) {
	s := WrapTest(TestWrapOptions{
		UserScript: "true", Package: "p@1",
		Home: "/h", Sandbox: "/box", PkgxBin: "pkgx", Host: linuxHost(),
	})
	if !strings.Contains(s, "exit 69") {
		t.Errorf("the test eval does not carry EnvFailExit:\n%s", s)
	}
	if strings.Contains(s, "  exit 1\n") {
		t.Errorf("the test eval still exits 1:\n%s", s)
	}
	b := Wrap(WrapOptions{
		UserScript: "make", Deps: []string{"zlib.net"},
		Target: linuxHost(), Host: linuxHost(), PkgxBin: "pkgx",
	})
	if !strings.Contains(b, "  exit 1\n") {
		t.Errorf("a BUILD must still exit 1 — the distinction is no use to it:\n%s", b)
	}
	if strings.Contains(b, "exit 69") {
		t.Errorf("a build took the test's status:\n%s", b)
	}
}
