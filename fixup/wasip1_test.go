package fixup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWasip1SkipsEverything: a .wasm module carries no ELF/Mach-O headers, so
// POSIX relocation is skipped as it is for windows.
func TestWasip1SkipsEverything(t *testing.T) {
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "lib", "pkgconfig", "x.pc"), "prefix="+prefix+"\n")
	write(t, filepath.Join(prefix, "bin", "tool.wasm"), "\x00asm\x01\x00\x00\x00")
	var logs []string
	if err := FixUp(Options{Prefix: prefix, Platform: "wasip1", Log: func(s string) { logs = append(logs, s) }}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(prefix, "lib", "pkgconfig", "x.pc"))
	if !strings.Contains(string(b), prefix) {
		t.Error("wasip1 must NOT rewrite .pc files")
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "wasip1 target: skipping POSIX") {
		t.Errorf("expected skip log, got %v", logs)
	}
	w, _ := os.ReadFile(filepath.Join(prefix, "bin", "tool.wasm"))
	if string(w) != "\x00asm\x01\x00\x00\x00" {
		t.Error("the module was modified")
	}
}
