package fixup

import (
	"os"
	"path/filepath"
	"testing"
)

// requireNonRoot skips permission-based error tests when running as root, where
// the kernel ignores the mode bits we rely on to force failures.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission-based error injection is a no-op for root")
	}
}

func TestFixPCFilesReadErrors(t *testing.T) {
	requireNonRoot(t)
	// unreadable .pc file → rewriteFile ReadFile error → propagates through FixUp
	prefix := t.TempDir()
	pc := filepath.Join(prefix, "lib", "pkgconfig", "x.pc")
	write(t, pc, "prefix="+prefix)
	os.Chmod(pc, 0)
	defer os.Chmod(pc, 0o644)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected error from unreadable .pc")
	}

	// unreadable pkgconfig dir → ReadDir error
	prefix2 := t.TempDir()
	d := filepath.Join(prefix2, "lib", "pkgconfig")
	write(t, filepath.Join(d, "y.pc"), "prefix="+prefix2)
	os.Chmod(d, 0)
	defer os.Chmod(d, 0o755)
	if err := FixUp(Options{Prefix: prefix2, Platform: "linux"}); err == nil {
		t.Error("expected error from unreadable pkgconfig dir")
	}
}

func TestRewriteFileWriteError(t *testing.T) {
	requireNonRoot(t)
	// readable but not writable .cmake with content to change → WriteFile fails
	prefix := t.TempDir()
	cm := filepath.Join(prefix, "lib", "cmake", "F.cmake")
	write(t, cm, "set(X \""+prefix+"\")")
	os.Chmod(cm, 0o400)
	defer os.Chmod(cm, 0o644)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected WriteFile permission error")
	}
}

func TestRemoveLaDirError(t *testing.T) {
	requireNonRoot(t)
	prefix := t.TempDir()
	lib := filepath.Join(prefix, "lib")
	write(t, filepath.Join(lib, "z.la"), "x")
	os.Chmod(lib, 0o500) // no write on dir → Remove fails
	defer os.Chmod(lib, 0o755)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected Remove error in read-only lib")
	}
}

func TestConsolidateLib64MkdirError(t *testing.T) {
	prefix := t.TempDir()
	// lib exists as a regular FILE → MkdirAll(lib) must fail
	write(t, filepath.Join(prefix, "lib"), "iam a file")
	write(t, filepath.Join(prefix, "lib64", "x.so"), "x")
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected MkdirAll error when lib is a file")
	}
}

func TestFlattenHeadersReadError(t *testing.T) {
	requireNonRoot(t)
	prefix := t.TempDir()
	sub := filepath.Join(prefix, "include", "only")
	write(t, filepath.Join(sub, "a.h"), "h")
	os.Chmod(sub, 0) // unreadable subdir → ReadDir(subdir) error
	defer os.Chmod(sub, 0o755)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected ReadDir error on unreadable include subdir")
	}
}

func TestFlattenHeadersRenameError(t *testing.T) {
	requireNonRoot(t)
	prefix := t.TempDir()
	inc := filepath.Join(prefix, "include")
	sub := filepath.Join(inc, "only")
	write(t, filepath.Join(sub, "a.h"), "h")
	os.Chmod(inc, 0o500) // can't create entries in include → Rename up fails
	defer os.Chmod(inc, 0o755)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected Rename error into read-only include")
	}
}

func TestSetRunpathOpenError(t *testing.T) {
	requireNonRoot(t)
	// a valid ELF that is not writable → OpenFile(O_RDWR) fails after parse
	p := buildELF64LE(t, "/opt/placeholder/aaaaaaaaaaaaaaaaaaaa", "libc.so.6", 40)
	os.Chmod(p, 0o400)
	defer os.Chmod(p, 0o644)
	err := SetRunpath(p, "$ORIGIN/x")
	if err == nil {
		t.Error("expected OpenFile RDWR permission error")
	}
}

func TestFixCMakeWalkError(t *testing.T) {
	requireNonRoot(t)
	prefix := t.TempDir()
	sub := filepath.Join(prefix, "lib", "cmake", "sub")
	write(t, filepath.Join(sub, "F.cmake"), "x")
	os.Chmod(sub, 0) // WalkDir invokes the callback with a permission error
	defer os.Chmod(sub, 0o755)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected WalkDir error under lib/cmake")
	}
}

func TestWalkExesReadError(t *testing.T) {
	requireNonRoot(t)
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(bin, 0) // WalkDir hits a permission error
	defer os.Chmod(bin, 0o755)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err == nil {
		t.Error("expected WalkDir error on unreadable bin")
	}
}
