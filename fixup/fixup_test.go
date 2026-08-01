package fixup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSkipsEverything(t *testing.T) {
	prefix := t.TempDir()
	// a .pc with an absolute path that would be rewritten on posix
	write(t, filepath.Join(prefix, "lib", "pkgconfig", "x.pc"), "prefix="+prefix+"\n")
	var logs []string
	err := FixUp(Options{Prefix: prefix, Platform: "windows", Log: func(s string) { logs = append(logs, s) }})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(prefix, "lib", "pkgconfig", "x.pc"))
	if !strings.Contains(string(b), prefix) {
		t.Error("windows must NOT rewrite .pc files")
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "skipping POSIX") {
		t.Errorf("expected windows-skip log, got %v", logs)
	}
}

func TestFixPCFiles(t *testing.T) {
	prefix := t.TempDir()
	build := prefix + "+brewing"
	pc := filepath.Join(prefix, "lib", "pkgconfig", "foo.pc")
	write(t, pc, "prefix="+build+"\nlibdir="+prefix+"/lib\nCflags: -I"+prefix+"/include\n")
	// a share/pkgconfig one too
	pc2 := filepath.Join(prefix, "share", "pkgconfig", "bar.pc")
	write(t, pc2, "prefix="+prefix+"\n")
	// a non-.pc file must be ignored
	write(t, filepath.Join(prefix, "lib", "pkgconfig", "note.txt"), prefix)

	if err := FixUp(Options{Prefix: prefix, BuildInstall: build, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(pc)
	s := string(b)
	if strings.Contains(s, prefix) || strings.Contains(s, build) {
		t.Errorf("pc not fully rewritten: %s", s)
	}
	if !strings.Contains(s, "${pcfiledir}/../..") {
		t.Errorf("pc missing pcfiledir-relative: %s", s)
	}
	if b2, _ := os.ReadFile(pc2); strings.Contains(string(b2), prefix) {
		t.Errorf("share/pc not rewritten: %s", b2)
	}
	if b3, _ := os.ReadFile(filepath.Join(prefix, "lib", "pkgconfig", "note.txt")); string(b3) != prefix {
		t.Error("non-.pc file was touched")
	}
}

func TestFixCMakeFiles(t *testing.T) {
	prefix := t.TempDir()
	cm := filepath.Join(prefix, "lib", "cmake", "Foo", "FooConfig.cmake")
	write(t, cm, "set(FOO_DIR \""+prefix+"/lib\")\n")
	// a non-.cmake file in the tree must be skipped (the `continue` branch)
	write(t, filepath.Join(prefix, "lib", "cmake", "Foo", "README.txt"), prefix)
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "lib", "cmake", "Foo", "README.txt")); string(b) != prefix {
		t.Error("non-.cmake file was rewritten")
	}
	b, _ := os.ReadFile(cm)
	if strings.Contains(string(b), prefix) {
		t.Errorf("cmake not rewritten: %s", b)
	}
	if !strings.Contains(string(b), "${CMAKE_CURRENT_LIST_DIR}") {
		t.Errorf("cmake missing token: %s", b)
	}
}

func TestRemoveLaFiles(t *testing.T) {
	prefix := t.TempDir()
	top := filepath.Join(prefix, "lib", "libfoo.la")
	sub := filepath.Join(prefix, "lib", "codecs", "plugin.la") // must survive
	write(t, top, "# la")
	write(t, sub, "# la")
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(top); !os.IsNotExist(err) {
		t.Error("top-level .la not removed")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Error("subdir .la wrongly removed")
	}
	// skip honoured
	write(t, top, "# la")
	var logs []string
	if err := FixUp(Options{Prefix: prefix, Platform: "linux", Skips: []string{"libtool-cleanup"}, Log: func(s string) { logs = append(logs, s) }}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(top); err != nil {
		t.Error("skip=libtool-cleanup should keep .la")
	}
}

func TestConsolidateLib64(t *testing.T) {
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "lib64", "libz.so"), "x")
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "lib", "libz.so")); err != nil {
		t.Error("lib64 content not moved to lib")
	}
	fi, err := os.Lstat(filepath.Join(prefix, "lib64"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("lib64 should now be a symlink")
	}
	// non-linux must not consolidate
	p2 := t.TempDir()
	write(t, filepath.Join(p2, "lib64", "x.so"), "x")
	if err := FixUp(Options{Prefix: p2, Platform: "darwin"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(p2, "lib64")); fi == nil || !fi.IsDir() {
		t.Error("darwin must not consolidate lib64")
	}
}

func TestFlattenHeaders(t *testing.T) {
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "include", "foo", "foo.h"), "h")
	write(t, filepath.Join(prefix, "include", "foo", "bar.h"), "h")
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "include", "foo.h")); err != nil {
		t.Error("headers not flattened up")
	}
	fi, err := os.Lstat(filepath.Join(prefix, "include", "foo"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("subdir should become a self-symlink")
	}
}

func TestFlattenHeadersSkipsShadow(t *testing.T) {
	prefix := t.TempDir()
	// a single subdir but it contains a system header → must NOT flatten
	write(t, filepath.Join(prefix, "include", "sys", "time.h"), "h")
	var logs []string
	if err := FixUp(Options{Prefix: prefix, Platform: "linux", Log: func(s string) { logs = append(logs, s) }}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "include", "time.h")); !os.IsNotExist(err) {
		t.Error("must not flatten a header that shadows a system header")
	}
	if !anyContains(logs, "would shadow") {
		t.Errorf("expected shadow-skip log: %v", logs)
	}
	// skip flatten entirely
	p2 := t.TempDir()
	write(t, filepath.Join(p2, "include", "only", "a.h"), "h")
	if err := FixUp(Options{Prefix: p2, Platform: "linux", Skips: []string{"flatten-includes"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p2, "include", "only")); err != nil {
		t.Error("skip=flatten-includes should leave subdir untouched")
	}
}

func TestFlattenHeadersMultipleEntriesNoop(t *testing.T) {
	prefix := t.TempDir()
	write(t, filepath.Join(prefix, "include", "a.h"), "h")
	write(t, filepath.Join(prefix, "include", "b", "b.h"), "h")
	if err := FixUp(Options{Prefix: prefix, Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	// two entries → no flatten, structure preserved
	if _, err := os.Stat(filepath.Join(prefix, "include", "a.h")); err != nil {
		t.Error("a.h vanished")
	}
	if _, err := os.Stat(filepath.Join(prefix, "include", "b", "b.h")); err != nil {
		t.Error("b/b.h vanished")
	}
}

func TestHelpersOnMissingDirs(t *testing.T) {
	// FixUp on an empty prefix must be a clean no-op across all steps.
	if err := FixUp(Options{Prefix: t.TempDir(), Platform: "linux"}); err != nil {
		t.Fatal(err)
	}
	if err := FixUp(Options{Prefix: t.TempDir(), Platform: "darwin"}); err != nil {
		t.Fatal(err)
	}
	if relTo("/a/b", "/a") != "b" {
		t.Error("relTo")
	}
	if !isDir(t.TempDir()) || isDir(filepath.Join(t.TempDir(), "nope")) {
		t.Error("isDir")
	}
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
