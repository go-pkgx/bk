//go:build darwin

package fixup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A real dylib, so the answer comes from what the linker wrote rather than from
// bytes this test made up. The install name carries a directory and the soname
// is only its last component.
func TestSonameOfMachO(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no cc")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "z.c")
	if err := os.WriteFile(src, []byte("int zed(void){ return 42; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dylib := filepath.Join(dir, "libz.1.2.3.dylib")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run(cc, "-dynamiclib", "-install_name", "@rpath/zlib.net/v1/lib/libz.1.dylib", "-o", dylib, src)
	raw, err := os.ReadFile(dylib)
	if err != nil {
		t.Fatal(err)
	}
	if got := SonameOf(raw); got != "libz.1.dylib" {
		t.Errorf("SonameOf = %q, want libz.1.dylib", got)
	}

	// An executable has no LC_ID_DYLIB, so nothing binds to it by name.
	exe := filepath.Join(dir, "prog")
	main := filepath.Join(dir, "main.c")
	if err := os.WriteFile(main, []byte("int main(void){ return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(cc, "-o", exe, main)
	raw, err = os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got := SonameOf(raw); got != "" {
		t.Errorf("SonameOf(executable) = %q, want empty", got)
	}
}
