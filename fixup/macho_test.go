package fixup

import (
	"debug/macho"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// machoCmd is a load command to embed in a crafted Mach-O.
type machoCmd struct {
	cmd uint32
	str string
}

// buildMachO writes a minimal thin Mach-O64 (LE) with the given string load
// commands — enough for debug/macho.Open to parse.
func buildMachO(t *testing.T, cmds ...machoCmd) string {
	t.Helper()
	le := binary.LittleEndian
	var body []byte
	for _, c := range cmds {
		strOff := 24
		if c.cmd == lcRpath {
			strOff = 12
		}
		size := strOff + len(c.str) + 1
		for size%8 != 0 {
			size++
		}
		cb := make([]byte, size)
		le.PutUint32(cb[0:], c.cmd)
		le.PutUint32(cb[4:], uint32(size))
		le.PutUint32(cb[8:], uint32(strOff))
		copy(cb[strOff:], c.str)
		body = append(body, cb...)
	}
	buf := make([]byte, 32+len(body))
	le.PutUint32(buf[0:], 0xfeedfacf) // MH_MAGIC_64
	le.PutUint32(buf[4:], 0x0100000c) // CPU_TYPE_ARM64
	le.PutUint32(buf[12:], 6)         // MH_DYLIB
	le.PutUint32(buf[16:], uint32(len(cmds)))
	le.PutUint32(buf[20:], uint32(len(body)))
	copy(buf[32:], body)
	p := filepath.Join(t.TempDir(), "obj.dylib")
	if err := os.WriteFile(p, buf, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// buildFatMachO wraps one crafted thin Mach-O64 slice per arches entry behind
// a 32-bit FAT_MAGIC universal header.
func buildFatMachO(t *testing.T, arches ...[]machoCmd) string {
	t.Helper()
	return buildFat(t, false, arches...)
}

// buildFat writes a fat (universal) Mach-O: magic + nfat_arch (uint32 BE),
// then one fat_arch entry per slice — 20 bytes (cputype, cpusubtype, offset,
// size, align, all uint32 BE) for FAT_MAGIC, 32 bytes (offset/size widened to
// uint64) for FAT_MAGIC_64 — followed by the 8-aligned thin slices. Each slice
// gets a distinct cputype: debug/macho rejects duplicate architectures.
func buildFat(t *testing.T, fat64 bool, arches ...[]machoCmd) string {
	t.Helper()
	be := binary.BigEndian
	var slices [][]byte
	for _, cmds := range arches {
		b, err := os.ReadFile(buildMachO(t, cmds...))
		if err != nil {
			t.Fatal(err)
		}
		slices = append(slices, b)
	}
	entry := 20
	magic := uint32(0xcafebabe) // FAT_MAGIC
	if fat64 {
		entry = 32
		magic = fatMagic64
	}
	buf := make([]byte, 8+entry*len(slices))
	be.PutUint32(buf[0:], magic)
	be.PutUint32(buf[4:], uint32(len(slices)))
	cpus := []uint32{0x0100000c, 0x01000007, 0x0000000c} // arm64, x86_64, arm
	for i, s := range slices {
		for len(buf)%8 != 0 { // 8-align each slice (align field = 2^3)
			buf = append(buf, 0)
		}
		off := len(buf)
		e := 8 + i*entry
		be.PutUint32(buf[e:], cpus[i]) // cpusubtype stays 0
		if fat64 {
			be.PutUint64(buf[e+8:], uint64(off))
			be.PutUint64(buf[e+16:], uint64(len(s)))
			be.PutUint32(buf[e+24:], 3)
		} else {
			be.PutUint32(buf[e+8:], uint32(off))
			be.PutUint32(buf[e+12:], uint32(len(s)))
			be.PutUint32(buf[e+16:], 3)
		}
		buf = append(buf, s...)
	}
	p := filepath.Join(t.TempDir(), "fat.dylib")
	if err := os.WriteFile(p, buf, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIsMachOAndRead(t *testing.T) {
	p := buildMachO(t,
		machoCmd{lcIDDylib, "/opt/x/v1+brewing/lib/libz.dylib"},
		machoCmd{lcLoadDylib, "/usr/lib/libSystem.B.dylib"},
		machoCmd{lcRpath, "/opt/pkgx"},
	)
	if !isMachO(p) {
		t.Fatal("crafted Mach-O not recognised")
	}
	ss, err := ReadMachoStrings(p)
	if err != nil || len(ss) != 3 || ss[0] != "/opt/x/v1+brewing/lib/libz.dylib" || ss[2] != "/opt/pkgx" {
		t.Fatalf("ReadMachoStrings = %v %v", ss, err)
	}
	// non-Mach-O
	bad := filepath.Join(t.TempDir(), "n")
	os.WriteFile(bad, []byte("not macho"), 0o644)
	if isMachO(bad) {
		t.Error("garbage is not Mach-O")
	}
	if _, err := ReadMachoStrings(bad); err == nil {
		t.Error("ReadMachoStrings should error on non-macho")
	}
}

func TestRewriteMachoStrings(t *testing.T) {
	p := buildMachO(t,
		machoCmd{lcIDDylib, "/opt/x/v1+brewing/lib/libz.dylib"},
		machoCmd{lcRpath, "/opt/pkgx"},
	)
	// strip +brewing (shorter → fits)
	if err := RewriteMachoStrings(p, func(s string) string { return strings.ReplaceAll(s, "+brewing", "") }); err != nil {
		t.Fatal(err)
	}
	ss, _ := ReadMachoStrings(p)
	if ss[0] != "/opt/x/v1/lib/libz.dylib" {
		t.Errorf("id not rewritten: %v", ss)
	}
	// still a valid Mach-O
	if !isMachO(p) {
		t.Error("rewrite corrupted the Mach-O")
	}
	// no-op fn → no write, no error
	if err := RewriteMachoStrings(p, func(s string) string { return s }); err != nil {
		t.Fatal(err)
	}
	// grow → ErrNoSpace
	if err := RewriteMachoStrings(p, func(s string) string { return s + "/way/too/long/to/fit/in/the/slot/xxxxxxxxxxxx" }); !errors.Is(err, ErrNoSpace) {
		t.Errorf("grow = %v want ErrNoSpace", err)
	}
	// non-macho
	bad := filepath.Join(t.TempDir(), "n")
	os.WriteFile(bad, []byte("no"), 0o644)
	if err := RewriteMachoStrings(bad, func(s string) string { return s }); err == nil {
		t.Error("expected error on non-macho")
	}
}

func TestRewriteMachoStringsSeamErrors(t *testing.T) {
	defer restore()
	p := buildMachO(t, machoCmd{lcIDDylib, "/opt/x/v1+brewing/lib/libz.dylib"})
	// readfile error in machoInfo
	osReadFile = func(string) ([]byte, error) { return nil, errInject }
	if err := RewriteMachoStrings(p, func(s string) string { return s }); !errors.Is(err, errInject) {
		t.Errorf("readfile seam: %v", err)
	}
	osReadFile = os.ReadFile
	// stat error (after a real change)
	osStat = func(string) (os.FileInfo, error) { return nil, errInject }
	if err := RewriteMachoStrings(p, func(s string) string { return strings.ReplaceAll(s, "+brewing", "") }); !errors.Is(err, errInject) {
		t.Errorf("stat seam: %v", err)
	}
	osStat = os.Stat
	// writefile error
	osWriteFile = func(string, []byte, os.FileMode) error { return errInject }
	if err := RewriteMachoStrings(p, func(s string) string { return strings.ReplaceAll(s, "+brewing", "") }); !errors.Is(err, errInject) {
		t.Errorf("writefile seam: %v", err)
	}
}

func TestRewriteMachoHelper(t *testing.T) {
	// BuildInstall empty → no-op
	if err := rewriteMacho("whatever", Options{}); err != nil {
		t.Fatal(err)
	}
	// non-macho → no-op
	bad := filepath.Join(t.TempDir(), "n")
	os.WriteFile(bad, []byte("no"), 0o644)
	if err := rewriteMacho(bad, Options{BuildInstall: "/b"}); err != nil {
		t.Fatal(err)
	}
	// real strip via FixUp(darwin): the crafted dylib under lib/ gets its
	// +brewing install name rewritten to the final prefix.
	prefix := t.TempDir()
	os.MkdirAll(filepath.Join(prefix, "lib"), 0o755)
	dy := buildMachO(t, machoCmd{lcIDDylib, prefix + "+brewing/lib/libz.dylib"})
	data, _ := os.ReadFile(dy)
	os.WriteFile(filepath.Join(prefix, "lib", "libz.dylib"), data, 0o755)
	if err := FixUp(Options{Prefix: prefix, BuildInstall: prefix + "+brewing", Platform: "darwin", Log: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	ss, _ := ReadMachoStrings(filepath.Join(prefix, "lib", "libz.dylib"))
	if len(ss) == 0 || strings.Contains(ss[0], "+brewing") {
		t.Errorf("darwin FixUp did not strip +brewing: %v", ss)
	}
	// ErrNoSpace is tolerated (logged, not fatal): a dylib whose install name is
	// "/x" and a buildInstall→prefix rewrite that GROWS it past its slot.
	prefix2 := t.TempDir()
	os.MkdirAll(filepath.Join(prefix2, "lib"), 0o755)
	dy2 := buildMachO(t, machoCmd{lcIDDylib, "/x"})
	d2, _ := os.ReadFile(dy2)
	os.WriteFile(filepath.Join(prefix2, "lib", "z.dylib"), d2, 0o755)
	var logs []string
	// Prefix is the real tree (so walkExes finds z.dylib); replacing "/x" with
	// the long prefix path overruns the fixed slot → ErrNoSpace, tolerated.
	if err := FixUp(Options{Prefix: prefix2, BuildInstall: "/x", Platform: "darwin", Log: func(s string) { logs = append(logs, s) }}); err != nil {
		t.Fatal(err)
	}
	if !anyContains(logs, "skip macho") {
		t.Errorf("expected tolerant skip log: %v", logs)
	}
}

func TestWalkMachoStringsEdges(t *testing.T) {
	le := binary.LittleEndian
	// a command with size < 8 → break (no panic, no change)
	raw := make([]byte, 32+8)
	le.PutUint32(raw[32:], lcIDDylib)
	le.PutUint32(raw[36:], 4) // bad size
	if changed, err := walkMachoStrings(raw, le, 32, 1, func(uint32, string) string { return "x" }); changed || err != nil {
		t.Errorf("size<8: %v %v", changed, err)
	}
	// a command whose size overruns the buffer → break
	raw = make([]byte, 32+8)
	le.PutUint32(raw[32:], lcIDDylib)
	le.PutUint32(raw[36:], 9999)
	if changed, _ := walkMachoStrings(raw, le, 32, 1, func(uint32, string) string { return "x" }); changed {
		t.Error("overrun should break")
	}
	// a string command whose strOff < 8 is skipped
	raw = make([]byte, 32+16)
	le.PutUint32(raw[32:], lcRpath)
	le.PutUint32(raw[36:], 16)
	le.PutUint32(raw[40:], 4) // strOff < 8
	if changed, _ := walkMachoStrings(raw, le, 32, 1, func(uint32, string) string { return "y" }); changed {
		t.Error("strOff<8 should be skipped")
	}
	// a non-string command is skipped, loop still advances
	raw = make([]byte, 32+8)
	le.PutUint32(raw[32:], 0x25) // LC_VERSION_MIN_MACOSX, not a string cmd
	le.PutUint32(raw[36:], 8)
	if changed, _ := walkMachoStrings(raw, le, 32, 1, func(uint32, string) string { return "z" }); changed {
		t.Error("non-string cmd should not change")
	}
	// a string command whose slot has NO NUL → cstr returns the whole region
	raw = make([]byte, 32+16)
	le.PutUint32(raw[32:], lcRpath)
	le.PutUint32(raw[36:], 16)
	le.PutUint32(raw[40:], 12)
	copy(raw[44:], []byte("abcd")) // 4 non-nul bytes, no terminator in the slot
	var seen string
	walkMachoStrings(raw, le, 32, 1, func(_ uint32, s string) string { seen = s; return s })
	if seen != "abcd" {
		t.Errorf("cstr no-nul = %q", seen)
	}
}

func TestFatMachO(t *testing.T) {
	p := buildFatMachO(t,
		[]machoCmd{{lcIDDylib, "/opt/x/v1+brewing/lib/libz.dylib"}, {lcRpath, "/opt/pkgx"}},
		[]machoCmd{{lcIDDylib, "/opt/x/v1+brewing/lib/libz.dylib"}},
	)
	// the crafted fat must be well-formed per the stdlib
	ff, err := macho.OpenFat(p)
	if err != nil {
		t.Fatalf("OpenFat on crafted fat: %v", err)
	}
	if len(ff.Arches) != 2 {
		t.Fatalf("arches = %d, want 2", len(ff.Arches))
	}
	ff.Close()
	if !isMachO(p) {
		t.Error("fat binary not recognised as Mach-O")
	}
	// strings from ALL slices
	ss, err := ReadMachoStrings(p)
	if err != nil || len(ss) != 3 {
		t.Fatalf("ReadMachoStrings fat = %v %v, want 3 strings", ss, err)
	}
	// rewrite touches every slice
	if err := RewriteMachoStrings(p, func(s string) string { return strings.ReplaceAll(s, "+brewing", "") }); err != nil {
		t.Fatal(err)
	}
	ss, _ = ReadMachoStrings(p)
	if len(ss) != 3 || ss[0] != "/opt/x/v1/lib/libz.dylib" || ss[1] != "/opt/pkgx" || ss[2] != "/opt/x/v1/lib/libz.dylib" {
		t.Errorf("fat rewrite = %v", ss)
	}
	// still a well-formed fat after the in-place rewrite
	if ff, err := macho.OpenFat(p); err != nil {
		t.Errorf("rewrite corrupted the fat: %v", err)
	} else {
		ff.Close()
	}
	// a slice whose replacement doesn't fit → ErrNoSpace
	if err := RewriteMachoStrings(p, func(s string) string { return s + "/way/too/long/to/fit/in/the/slot/xxxxxxxxxxxx" }); !errors.Is(err, ErrNoSpace) {
		t.Errorf("fat grow = %v want ErrNoSpace", err)
	}
}

func TestFat64MachO(t *testing.T) {
	p := buildFat(t, true,
		[]machoCmd{{lcIDDylib, "/opt/x/v1+brewing/lib/liba.dylib"}},
		[]machoCmd{{lcRpath, "/opt/pkgx+brewing"}},
	)
	if !isMachO(p) {
		t.Error("fat64 binary not recognised as Mach-O")
	}
	ss, err := ReadMachoStrings(p)
	if err != nil || len(ss) != 2 || ss[0] != "/opt/x/v1+brewing/lib/liba.dylib" || ss[1] != "/opt/pkgx+brewing" {
		t.Fatalf("ReadMachoStrings fat64 = %v %v", ss, err)
	}
	if err := RewriteMachoStrings(p, func(s string) string { return strings.ReplaceAll(s, "+brewing", "") }); err != nil {
		t.Fatal(err)
	}
	ss, _ = ReadMachoStrings(p)
	if len(ss) != 2 || ss[0] != "/opt/x/v1/lib/liba.dylib" || ss[1] != "/opt/pkgx" {
		t.Errorf("fat64 rewrite = %v", ss)
	}
}

func TestFatMachOBadInputs(t *testing.T) {
	be := binary.BigEndian
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// FAT_MAGIC with zero images → macho.NewFatFile error
	b := make([]byte, 8)
	be.PutUint32(b, 0xcafebabe)
	p := write("empty", b)
	if isMachO(p) {
		t.Error("no-image fat should not be Mach-O")
	}
	if _, err := ReadMachoStrings(p); err == nil {
		t.Error("expected error on no-image fat")
	}
	// FAT_MAGIC_64 whose fat_arch_64 table is truncated
	b = make([]byte, 12)
	be.PutUint32(b, fatMagic64)
	be.PutUint32(b[4:], 1)
	p = write("trunc", b)
	if _, err := ReadMachoStrings(p); err == nil {
		t.Error("expected error on truncated fat_arch_64 table")
	}
	// FAT_MAGIC_64 whose slice offset/size point outside the file
	b = make([]byte, 8+32)
	be.PutUint32(b, fatMagic64)
	be.PutUint32(b[4:], 1)
	be.PutUint64(b[8+8:], 1<<40) // offset far past EOF
	be.PutUint64(b[8+16:], 64)
	p = write("oob", b)
	if isMachO(p) {
		t.Error("out-of-bounds slice should not be Mach-O")
	}
	if _, err := ReadMachoStrings(p); err == nil {
		t.Error("expected error on out-of-bounds fat slice")
	}
}
