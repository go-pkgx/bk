package fixup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditRelocatableFindsTheBuilderOnlyRpath: a mirrored bottle is never
// unpacked, so FixUp's guards never see it. The audit is the same guards run
// read-only over a laid-out prefix, which is what a mirror can afford.
//
// The shape is git-scm.org 2.55.0's: a correctly QUALIFIED @rpath reference to
// a sibling package, and one rpath — the build machine's own absolute path.
func TestAuditRelocatableFindsTheBuilderOnlyRpath(t *testing.T) {
	pkgxDir := t.TempDir()
	prefix := filepath.Join(pkgxDir, "git-scm.org", "v2.55.0")
	ref := machoCmd{lcLoadDylib, "@rpath/zlib.net/v1.3.2/lib/libz.1.dylib"}

	// libexec/git: reaches the sibling only through the builder's path.
	// walkExes looks in bin, lib and libexec — an audit that read only bin/
	// would have missed git-scm.org, whose broken files are under libexec/.
	bad := filepath.Join(prefix, "libexec", "git")
	place(t, bad, ref, machoCmd{lcRpath, "/Users/runner/.pkgx"})
	// lib/libok.dylib: a relative rpath that reaches $PKGX_DIR from lib/.
	place(t, filepath.Join(prefix, "lib", "libok.dylib"), ref, machoCmd{lcRpath, "@loader_path/../../.."})

	checked, problems := AuditRelocatable(prefix, pkgxDir)
	if checked != 2 {
		t.Fatalf("checked %d Mach-O, want 2", checked)
	}
	if len(problems) != 1 {
		t.Fatalf("reported %d problem(s), want 1: %v", len(problems), problems)
	}
	if !errors.Is(problems[0], ErrBuilderOnlyRpath) {
		t.Errorf("problem is %v, want ErrBuilderOnlyRpath", problems[0])
	}
	if !strings.Contains(problems[0].Error(), bad) {
		t.Errorf("problem does not name %s: %v", bad, problems[0])
	}
}

// TestAuditRelocatableIsQuietOnAHealthyPrefix: the negative control. Without
// it the audit could report every bottle and still look like it works.
func TestAuditRelocatableIsQuietOnAHealthyPrefix(t *testing.T) {
	pkgxDir := t.TempDir()
	prefix := filepath.Join(pkgxDir, "lloyd.github.io", "yajl", "v2.1.0")
	place(t, filepath.Join(prefix, "lib", "libyajl.2.dylib"),
		machoCmd{lcLoadDylib, "@rpath/zlib.net/v1.3.2/lib/libz.1.dylib"},
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcRpath, "/Users/runner/.pkgx"})

	checked, problems := AuditRelocatable(prefix, pkgxDir)
	if checked != 1 {
		t.Fatalf("checked %d Mach-O, want 1", checked)
	}
	if len(problems) != 0 {
		t.Errorf("reported %v on a prefix whose relative rpath reaches $PKGX_DIR", problems)
	}
}

// TestAuditRelocatableReadsNothingItCannotParse: a prefix with no Mach-O at all
// is not an error — a mirrored bottle can be pure scripts or data.
func TestAuditRelocatableSkipsNonMachO(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "git"), []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	checked, problems := AuditRelocatable(prefix, prefix)
	if checked != 0 || len(problems) != 0 {
		t.Errorf("checked=%d problems=%v, want 0 and none", checked, problems)
	}
}

// The audit refuses a Mach-O that names one rpath twice. ld will not link
// against such a library, so the damage lands on every dependent rather than on
// the bottle carrying it — which is why a duplicate survived a build, a
// signature, a publish and an inspection without anything complaining, until
// github.com/facebookincubator/fizz failed to link against folly.
func TestAuditRefusesADuplicateRpath(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "acme.org", "foo", "v1.0.0")
	exe := filepath.Join(prefix, "lib", "libfoo.dylib")
	place(t, exe,
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcLoadDylib, "@rpath/other.org/bar/v1/lib/libbar.dylib"},
	)
	checked, problems := AuditRelocatable(prefix, pkgx)
	if checked != 1 {
		t.Fatalf("checked %d files, want 1", checked)
	}
	var found error
	for _, p := range problems {
		if errors.Is(p, ErrDuplicateRpath) {
			found = p
		}
	}
	if found == nil {
		t.Fatalf("no duplicate reported; problems = %v", problems)
	}
	// The message has to name the file AND the repeated entry, or the reader
	// has to go looking for which of them it is.
	if !strings.Contains(found.Error(), "libfoo.dylib") ||
		!strings.Contains(found.Error(), "@loader_path/../../../..") {
		t.Errorf("message hides half the problem: %v", found)
	}
}

// One rpath named once is not a duplicate, however many other rpaths there are.
func TestAuditAcceptsDistinctRpaths(t *testing.T) {
	pkgx := filepath.Join(t.TempDir(), ".pkgx")
	prefix := filepath.Join(pkgx, "acme.org", "foo", "v1.0.0")
	exe := filepath.Join(prefix, "lib", "libfoo.dylib")
	place(t, exe,
		machoCmd{lcRpath, "@loader_path/../../../.."},
		machoCmd{lcRpath, "@loader_path/../../../../.."},
		machoCmd{lcLoadDylib, "@rpath/other.org/bar/v1/lib/libbar.dylib"},
	)
	_, problems := AuditRelocatable(prefix, pkgx)
	for _, p := range problems {
		if errors.Is(p, ErrDuplicateRpath) {
			t.Errorf("distinct rpaths reported as duplicate: %v", p)
		}
	}
}
