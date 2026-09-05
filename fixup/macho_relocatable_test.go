package fixup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// place writes a crafted Mach-O at an arbitrary path — buildMachO picks its
// own, and where a binary SITS is half of what the rewrite decides.
func place(t *testing.T, path string, cmds ...machoCmd) {
	t.Helper()
	b, err := os.ReadFile(buildMachO(t, cmds...))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// An install name pointing into $PKGX_DIR becomes an @rpath reference, so the
// bottle resolves it wherever $PKGX_DIR turns out to be.
func TestInstallNamesBecomeRpathReferences(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "other.org", "bar", "v2.0.0")
	exe := filepath.Join(prefix, "bin", "bar")
	place(t, exe,
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcLoadDylib, filepath.Join(pkgx, "acme.org/foo/v1.2.3/lib/libfoo.dylib")},
		machoCmd{lcLoadDylib, "/usr/lib/libSystem.B.dylib"},
	)
	if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: pkgx}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMachoStrings(exe)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"@loader_path/../../../..",                // an rpath is a search root, not a reference
		"@rpath/acme.org/foo/v1/lib/libfoo.dylib", // major-versioned: v1 is a symlink pkgx maintains
		"/usr/lib/libSystem.B.dylib",              // the OS is not in $PKGX_DIR
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("string %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The same file with an rpath that does NOT reach $PKGX_DIR keeps its absolute
// names: @rpath would resolve to nothing, which is worse than a path that at
// least works on one machine.
func TestNoReachingRpathLeavesNamesAbsolute(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "other.org", "bar", "v2.0.0")
	exe := filepath.Join(prefix, "bin", "bar")
	dep := filepath.Join(pkgx, "acme.org/foo/v1.2.3/lib/libfoo.dylib")
	place(t, exe,
		machoCmd{lcRpath, "@loader_path/.."}, // stops at the version dir
		machoCmd{lcLoadDylib, dep},
	)
	var logged []string
	if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: pkgx,
		Log: func(s string) { logged = append(logged, s) }}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMachoStrings(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != dep {
		t.Errorf("string = %q, want it left at %q", got[1], dep)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "no rpath reaching") {
		t.Errorf("the refusal was silent:\n%s", strings.Join(logged, "\n"))
	}
}

// Every shape an rpath can take, and whether it reaches $PKGX_DIR from a
// binary installed at <pkgx>/other.org/bar/v2.0.0/bin/bar.
func TestRpathReaches(t *testing.T) {
	pkgx := "/opt/.pkgx"
	exe := pkgx + "/other.org/bar/v2.0.0/bin/bar"
	for _, tc := range []struct {
		name  string
		rpath string
		want  bool
	}{
		{"relative, right depth", "@loader_path/../../../..", true},
		{"relative, too shallow", "@loader_path/../..", false},
		{"the executable's own path", "@executable_path/../../../..", true},
		{"absolute, the same dir", pkgx, true},
		{"absolute, another dir", "/usr/local/lib", false},
		{"neither absolute nor anchored", "lib", false},
		{"@rpath itself, which anchors nothing", "@rpath/lib", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rpathReaches(exe, pkgx, []string{tc.rpath}); got != tc.want {
				t.Errorf("rpathReaches(%q) = %v, want %v", tc.rpath, got, tc.want)
			}
		})
	}
	if rpathReaches(exe, pkgx, nil) {
		t.Error("no rpath at all reached somewhere")
	}
}

// underDir must not accept a sibling that merely starts with the same letters:
// /opt/.pkgxdirty is not inside /opt/.pkgx.
func TestUnderDir(t *testing.T) {
	for _, tc := range []struct {
		p, dir, rest string
		ok           bool
	}{
		{"/opt/.pkgx/acme.org/x", "/opt/.pkgx", "acme.org/x", true},
		{"/opt/.pkgxdirty/acme.org/x", "/opt/.pkgx", "", false},
		{"/opt/.pkgx", "/opt/.pkgx", "", false},
		{"/opt/.pkgx/", "/opt/.pkgx", "", false},
		{"/elsewhere/x", "/opt/.pkgx", "", false},
		{"/opt/.pkgx/x", "", "", false},
		{"/opt/.pkgx/acme.org/x", "/opt/.pkgx/", "acme.org/x", true},
	} {
		rest, ok := underDir(tc.p, tc.dir)
		if ok != tc.ok || rest != tc.rest {
			t.Errorf("underDir(%q, %q) = %q, %v; want %q, %v", tc.p, tc.dir, rest, ok, tc.rest, tc.ok)
		}
	}
}

// Without a $PKGX_DIR — `bk fixup <prefix>` run by hand — the pass does what it
// always did: strip the staging prefix, and nothing else.
func TestNoPkgxDirIsTheOldBehaviour(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "acme.org", "foo", "v1.2.3")
	exe := filepath.Join(prefix, "bin", "foo")
	place(t, exe, machoCmd{lcIDDylib, prefix + "+brewing/lib/libfoo.dylib"})
	if err := FixUp(Options{Prefix: prefix, BuildInstall: prefix + "+brewing", Platform: "darwin"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMachoStrings(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != prefix+"/lib/libfoo.dylib" {
		t.Errorf("string = %q", got[0])
	}
}

// A file that is not a Mach-O at all, and one with nothing to rewrite, must
// both come back untouched rather than rewritten or refused.
func TestNothingToDo(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "acme.org", "foo", "v1.2.3")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(prefix, "bin", "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(prefix, "bin", "foo")
	place(t, exe, machoCmd{lcLoadDylib, "/usr/lib/libSystem.B.dylib"})
	before, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: dir}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a Mach-O with nothing to rewrite was written back")
	}
}

// An unreadable Mach-O must surface as an error rather than as a file quietly
// left absolute — the read that fails here is the one asking what rpaths it has.
func TestRpathReadErrorSurfaces(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "other.org", "bar", "v2.0.0")
	exe := filepath.Join(prefix, "bin", "bar")
	place(t, exe, machoCmd{lcRpath, "@loader_path/../../../.."})
	defer restoreReadFile(os.ReadFile)
	calls := 0
	osReadFile = func(p string) ([]byte, error) {
		calls++
		if calls > 1 { // the first read is isMachO's
			return nil, errInject
		}
		return os.ReadFile(p)
	}
	if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: pkgx}); err == nil {
		t.Fatal("want the read error, got none")
	}
}

// A dependency's MINOR upgrade must not orphan its dependents, which is why the
// reference is major-versioned. Measured on the published gnu.org/grep bottle:
// it named /Users/runner/.pkgx/pcre.org/v2/v10.47/lib/libpcre2-8.0.dylib, the
// machine had pcre2 v10.48, grep could not start — and curl's configure
// reported it as "'grep' utility not found in 'PATH'".
//
// A package's OWN libraries keep their full version: they ship together and
// cannot disagree with each other.
func TestDependencyVersionsAreMajorOnly(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "gnu.org", "grep", "v3.12")
	exe := filepath.Join(prefix, "bin", "grep")
	place(t, exe,
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcLoadDylib, filepath.Join(pkgx, "pcre.org/v2/v10.47/lib/libpcre2-8.0.dylib")},
		machoCmd{lcIDDylib, filepath.Join(prefix, "lib/libgreputils.1.dylib")},
	)
	if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: pkgx}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMachoStrings(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != "@rpath/pcre.org/v2/v10/lib/libpcre2-8.0.dylib" {
		t.Errorf("dependency = %q, want it major-versioned", got[1])
	}
	if got[2] != "@rpath/gnu.org/grep/v3.12/lib/libgreputils.1.dylib" {
		t.Errorf("own library = %q, want its full version kept", got[2])
	}
}

// Once a dependency is itself built with an @rpath install name, its dependents
// record @rpath/<project>/v<FULL version>/… — which never reached the
// absolute-path branch, so the version stayed whole and the reference breaks on
// the dependency's next minor release. Measured on the grep bottle rebuilt
// after pcre2 had been: @rpath/pcre.org/v2/v10.48/lib/libpcre2-8.0.dylib.
func TestAlreadyRpathReferencesAreMajorVersionedToo(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "gnu.org", "grep", "v3.12")
	exe := filepath.Join(prefix, "bin", "grep")
	place(t, exe,
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcLoadDylib, "@rpath/pcre.org/v2/v10.48/lib/libpcre2-8.0.dylib"},
		machoCmd{lcLoadDylib, "@rpath/gnu.org/grep/v3.12/lib/libgreputils.1.dylib"},
		machoCmd{lcLoadDylib, "@rpath/not/under/anything.dylib"},
	)
	if err := FixUp(Options{Prefix: prefix, Platform: "darwin", PkgxDir: pkgx}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMachoStrings(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "@loader_path/../../../.." {
		t.Errorf("the rpath itself was rewritten: %q", got[0])
	}
	if got[1] != "@rpath/pcre.org/v2/v10/lib/libpcre2-8.0.dylib" {
		t.Errorf("dependency = %q, want it major-versioned", got[1])
	}
	if got[2] != "@rpath/gnu.org/grep/v3.12/lib/libgreputils.1.dylib" {
		t.Errorf("own library = %q, want its full version kept", got[2])
	}
	if got[3] != "@rpath/not/under/anything.dylib" {
		t.Errorf("a reference with no version = %q, want it untouched", got[3])
	}
}
