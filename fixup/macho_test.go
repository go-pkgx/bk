package fixup

import (
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
