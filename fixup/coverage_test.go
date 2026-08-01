package fixup

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readDirErr covers the osReadDir error return in fixPCFiles, removeLaFiles and
// flattenHeaders — each is the first ReadDir reached for its tailored prefix.
func TestSeamReadDirErrors(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"pc": pcPrefix,
		"la": func(t *testing.T) string {
			p := t.TempDir()
			write(t, filepath.Join(p, "lib", "z.la"), "x")
			return p
		},
		"cmake": func(t *testing.T) string {
			p := t.TempDir()
			write(t, filepath.Join(p, "lib", "cmake", "F.cmake"), "x")
			return p
		},
		"include": incPrefix,
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			defer restore()
			osReadDir = func(string) ([]os.DirEntry, error) { return nil, errInject }
			if err := FixUp(Options{Prefix: mk(t), Platform: "linux"}); err == nil {
				t.Errorf("%s: expected ReadDir seam error", name)
			}
		})
	}
}

func TestWalkCMakeRecursiveError(t *testing.T) {
	defer restore()
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "lib", "cmake", "sub", "F.cmake"), "x")
	// error only when recursing into the nested subdir
	osReadDir = func(p string) ([]os.DirEntry, error) {
		if strings.HasSuffix(p, "sub") {
			return nil, errInject
		}
		return os.ReadDir(p)
	}
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected recursive walkCMake error")
	}
}

func TestWalkCMakeRewriteError(t *testing.T) {
	defer restore()
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "lib", "cmake", "F.cmake"), "set(X \""+prefix+"\")")
	osWriteFile = func(string, []byte, os.FileMode) error { return errInject }
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected rewriteFile error inside walkCMake")
	}
}

func TestRewriteFileNoChange(t *testing.T) {
	// a .pc with nothing to rewrite exercises rewriteFile's clean return nil.
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "lib", "pkgconfig", "x.pc"), "Name: foo\nVersion: 1\n")
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(prefix, "lib", "pkgconfig", "x.pc"))
	if !strings.Contains(string(b), "Name: foo") {
		t.Error("unchanged file corrupted")
	}
}

func TestFixUpUnknownPlatform(t *testing.T) {
	// a non-linux/darwin/windows platform hits fixRpaths' default branch.
	if err := FixUp(Options{Prefix: t.TempDir(), Platform: "openbsd"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunpathStrOffsetNoNull(t *testing.T) {
	// no rpath and no DT_NULL → the loop falls through to the final return.
	le := binary.LittleEndian
	var d [16]byte
	le.PutUint64(d[0:], uint64(elf.DT_NEEDED))
	le.PutUint64(d[8:], 5)
	if _, ok := runpathStrOffset(d[:], elf.ELFCLASS64, le); ok {
		t.Error("expected no offset")
	}
}

func TestSetRunpathNoDynamic(t *testing.T) {
	// a valid ELF with no .dynamic section → ErrNoRunpath (dynSec nil branch).
	p := buildBareELF(t)
	if err := SetRunpath(p, "x"); err != ErrNoRunpath {
		t.Errorf("bare ELF: %v want ErrNoRunpath", err)
	}
}

func TestSetRunpathDataError(t *testing.T) {
	// a .dynamic section whose sh_size runs past EOF makes Section.Data() fail.
	p := buildELF64LE(t, "/opt/x/aaaa", "libc.so.6", 16)
	corruptSectionSize(t, p, ".dynamic")
	if err := SetRunpath(p, "y"); err == nil {
		t.Error("expected Data() error on oversized .dynamic")
	}
}

func TestRewriteRunpathEmptyResult(t *testing.T) {
	// an ELF whose only RUNPATH entry is a foreign abs path (dropped) with no
	// deps yields an empty rpath set → rewriteRunpath returns early (no write).
	prefix := filepath.Join(t.TempDir(), "acme.org", "tool", "v1.0.0")
	os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	src := buildELF64LE(t, "/foreign/only/path", "libc.so.6", 32)
	data, _ := os.ReadFile(src)
	exe := filepath.Join(prefix, "bin", "tool")
	os.WriteFile(exe, data, 0o755)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	// unchanged: still the foreign path (nothing was rewritten)
	if rp, _ := ReadRunpath(exe); rp != "/foreign/only/path" {
		t.Errorf("expected untouched runpath, got %q", rp)
	}
}

func TestRewriteRunpathTolerantNoSpace(t *testing.T) {
	// a short RUNPATH slot + a dep that cannot fit → ErrNoSpace is tolerated
	// (logged and skipped, FixUp still succeeds).
	prefix := filepath.Join(t.TempDir(), "acme.org", "tool", "v1.0.0")
	os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	src := buildELF64LE(t, "$ORIGIN", "libc.so.6", 7) // 7-byte slot
	data, _ := os.ReadFile(src)
	exe := filepath.Join(prefix, "bin", "tool")
	os.WriteFile(exe, data, 0o755)
	var logs []string
	err := FixUp(Options{
		Prefix: prefix, Platform: "linux",
		DepPaths: []string{"/opt/pkgx/dep.org/x/v9.9.9/lib/way/too/long/to/fit"},
		Log:      func(s string) { logs = append(logs, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !anyContains(logs, "skip rpath") {
		t.Errorf("expected tolerant skip log, got %v", logs)
	}
}

// buildBareELF writes an ELF64-LE with only NULL + .shstrtab sections (no
// .dynamic), which debug/elf accepts.
func buildBareELF(t *testing.T) string {
	t.Helper()
	le := binary.LittleEndian
	shstr := []byte("\x00.shstrtab\x00")
	const eh = 64
	shstrOff := int64(eh)
	shoff := shstrOff + int64(len(shstr))
	if r := shoff % 8; r != 0 {
		shoff += 8 - r
	}
	buf := make([]byte, shoff+2*64)
	copy(buf, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	le.PutUint16(buf[16:], uint16(elf.ET_DYN))
	le.PutUint16(buf[18:], uint16(elf.EM_X86_64))
	le.PutUint32(buf[20:], 1)
	le.PutUint64(buf[40:], uint64(shoff))
	le.PutUint16(buf[52:], eh)
	le.PutUint16(buf[58:], 64)
	le.PutUint16(buf[60:], 2)
	le.PutUint16(buf[62:], 1)
	copy(buf[shstrOff:], shstr)
	b := buf[shoff+64:]
	le.PutUint32(b[0:], 1) // sh_name → ".shstrtab"
	le.PutUint32(b[4:], uint32(elf.SHT_STRTAB))
	le.PutUint64(b[24:], uint64(shstrOff))
	le.PutUint64(b[32:], uint64(len(shstr)))
	le.PutUint64(b[48:], 1)
	p := filepath.Join(t.TempDir(), "bare.so")
	if err := os.WriteFile(p, buf, 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := elf.Open(p); err != nil {
		t.Fatalf("bare ELF invalid: %v", err)
	} else {
		f.Close()
	}
	return p
}

// corruptSectionSize inflates a named section's sh_size beyond EOF so that
// Section.Data() returns an error, without disturbing the header parse.
func corruptSectionSize(t *testing.T, path, section string) {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// locate the section index + the e_shoff/e_shentsize from the header bytes
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	e_shoff := int64(le.Uint64(raw[40:]))
	shentsize := int64(le.Uint16(raw[58:]))
	shnum := int(le.Uint16(raw[60:]))
	var idx = -1
	for i, s := range f.Sections {
		if s.Name == section {
			idx = i
		}
	}
	f.Close()
	if idx < 0 || idx >= shnum {
		t.Fatalf("section %s not found", section)
	}
	// sh_size is at offset 32 within a 64-byte Elf64_Shdr
	pos := e_shoff + int64(idx)*shentsize + 32
	le.PutUint64(raw[pos:], 1<<40) // absurdly large → past EOF
	if err := os.WriteFile(path, raw, 0o755); err != nil {
		t.Fatal(err)
	}
}
