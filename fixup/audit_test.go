package fixup

import (
	"encoding/binary"
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

// A 32-bit magic over a 64-bit cputype is what GNU strip leaves behind, and no
// linker produces it. github.com/rcedgar/muscle 5.3 shipped that way for
// darwin/aarch64 and the kernel kills it on sight — exit 137, no output.
//
// The existing guards caught that file only sideways, through the LC_RPATH it
// no longer had, because strip took the load commands too. The malformation
// itself is four bytes wide and needs naming: debug/macho parses such a file
// happily AS 32-bit, so every offset read afterwards is wrong.
func TestAuditRelocatableFindsAMagicThatContradictsTheCPU(t *testing.T) {
	p := buildThin32WithA64BitCPU(t)
	if err := checkMachoMagic(p); !errors.Is(err, ErrMachoMagicMismatch) {
		t.Fatalf("err = %v, want ErrMachoMagicMismatch", err)
	}
	// and the audit reports it over a laid-out prefix, which is the only way a
	// MIRRORED bottle is ever looked at — walkExes visits bin/, lib/ and
	// libexec/, so the file has to be in one of them
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "lib", "libbad.dylib"), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	_, problems := AuditRelocatable(prefix, t.TempDir())
	found := false
	for _, e := range problems {
		if errors.Is(e, ErrMachoMagicMismatch) {
			found = true
		}
	}
	if !found {
		t.Errorf("AuditRelocatable problems = %v, want the magic mismatch among them", problems)
	}
}

// buildThin32WithA64BitCPU writes a file that is structurally a 32-bit Mach-O —
// 28-byte header, one LC_RPATH — whose cputype carries CPU_ARCH_ABI64.
//
// Flipping the magic byte of a 64-bit fixture is NOT the same thing and does
// not reproduce it: the load commands then start four bytes past where a
// 32-bit reader looks, and the parse fails before the header can be judged.
// The published muscle binary parses cleanly as 32-bit, which is exactly why
// nothing downstream noticed.
func buildThin32WithA64BitCPU(t *testing.T) string {
	t.Helper()
	le := binary.LittleEndian
	const path = "@loader_path/../lib"
	cmdSize := 12 + len(path) + 1
	for cmdSize%4 != 0 {
		cmdSize++
	}
	cmd := make([]byte, cmdSize)
	le.PutUint32(cmd[0:], lcRpath)
	le.PutUint32(cmd[4:], uint32(cmdSize))
	le.PutUint32(cmd[8:], 12)
	copy(cmd[12:], path)

	buf := make([]byte, 28+len(cmd))
	le.PutUint32(buf[0:], 0xfeedface) // MH_MAGIC — 32-bit
	le.PutUint32(buf[4:], 0x0100000c) // CPU_TYPE_ARM64 — 64-bit
	le.PutUint32(buf[12:], 6)         // MH_DYLIB
	le.PutUint32(buf[16:], 1)         // ncmds
	le.PutUint32(buf[20:], uint32(len(cmd)))
	copy(buf[28:], cmd)

	q := filepath.Join(t.TempDir(), "thin32-over-64.dylib")
	if err := os.WriteFile(q, buf, 0o755); err != nil {
		t.Fatal(err)
	}
	return q
}

// And it must stay quiet on a healthy file, including a genuine 32-bit one:
// the check is about DISAGREEMENT between the header and the cputype, not
// about the word size.
func TestCheckMachoMagicAcceptsAgreementBothWays(t *testing.T) {
	p := buildMachO(t, machoCmd{lcRpath, "@loader_path/../lib"})
	if err := checkMachoMagic(p); err != nil {
		t.Errorf("64-bit magic + 64-bit cputype: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 0xce // 32-bit magic
	raw[7] = 0x00 // cputype 0x0000000c — CPU_TYPE_ARM, the 32-bit one
	q := filepath.Join(t.TempDir(), "thin32.dylib")
	if err := os.WriteFile(q, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMachoMagic(q); err != nil && errors.Is(err, ErrMachoMagicMismatch) {
		t.Errorf("32-bit magic + 32-bit cputype must be accepted: %v", err)
	}
}

// A file that is not a Mach-O at all is the reader's problem, not this check's:
// the error is returned rather than mistaken for a malformation.
func TestCheckMachoMagicOnSomethingElse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-macho")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMachoMagic(p); err == nil || errors.Is(err, ErrMachoMagicMismatch) {
		t.Errorf("err = %v, want a parse error and not a magic mismatch", err)
	}
}

// placeSigned writes a signed image at path, so an audit test can put one
// inside a prefix rather than in a bare temp dir.
func placeSigned(t *testing.T, path string, img signedImage) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, img.raw, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestAuditRelocatableFindsTheStaleSignature: the check that existed, was
// tested, and was wired to nothing.
//
// Every other guard here reads a Mach-O *reference*. None of them asks whether
// the bytes still hash to what the signature claims, so 2123 files across 56
// published project@versions went out in a state where Apple silicon SIGKILLs
// them at the first page fault — with no dyld error and no output at all.
func TestAuditRelocatableFindsTheStaleSignature(t *testing.T) {
	pkgxDir := t.TempDir()
	prefix := filepath.Join(pkgxDir, "gnu.org", "libtool", "v2.6.2")

	bad := filepath.Join(prefix, "lib", "libltdl.7.dylib")
	placeSigned(t, bad, buildSignedMachO(t, "@loader_path/../../../..", sigOpts{badHashes: true}))
	placeSigned(t, filepath.Join(prefix, "lib", "libok.dylib"),
		buildSignedMachO(t, "@loader_path/../../../..", sigOpts{}))

	checked, problems := AuditRelocatable(prefix, pkgxDir)
	if checked != 2 {
		t.Fatalf("checked %d Mach-O, want 2", checked)
	}
	if len(problems) != 1 {
		t.Fatalf("reported %d problem(s), want 1: %v", len(problems), problems)
	}
	if !errors.Is(problems[0], ErrStaleSignature) {
		t.Errorf("problem is %v, want ErrStaleSignature", problems[0])
	}
	// Naming the file is the point: the operator's next move is to rebuild
	// THAT package, and a prefix-wide complaint does not say which.
	if !strings.Contains(problems[0].Error(), bad) {
		t.Errorf("problem does not name %s: %v", bad, problems[0])
	}
}

// TestAuditRelocatableSurfacesAnUnreadableSignature: a signature bk cannot
// compute is reported, not silently passed. Reading "no verdict" as "sound" is
// how an unrunnable bottle gets published.
func TestAuditRelocatableSurfacesAnUnreadableSignature(t *testing.T) {
	pkgxDir := t.TempDir()
	prefix := filepath.Join(pkgxDir, "x", "v1")

	img := buildSignedMachO(t, "@loader_path/../../..", sigOpts{})
	img.raw[img.cdOff+37] = 99 // a hash algorithm we cannot compute
	placeSigned(t, filepath.Join(prefix, "lib", "libx.dylib"), img)

	_, problems := AuditRelocatable(prefix, pkgxDir)
	if len(problems) != 1 {
		t.Fatalf("reported %d problem(s), want 1: %v", len(problems), problems)
	}
	if errors.Is(problems[0], ErrStaleSignature) {
		t.Errorf("called an unreadable signature stale: %v", problems[0])
	}
}
