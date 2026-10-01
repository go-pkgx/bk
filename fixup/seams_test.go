package fixup

import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errInject = errors.New("injected")

// restore resets all seams to the real os functions after a test.
func restore() {
	osStat, osReadDir, osWriteFile = os.Stat, os.ReadDir, os.WriteFile
	osRename, osRemove, osSymlink = os.Rename, os.Remove, os.Symlink
	osMkdirAll, osOpenFile = os.MkdirAll, os.OpenFile
}

// pcPrefix builds a prefix with one rewritable .pc file (drives rewriteFile).
func pcPrefix(t *testing.T) string {
	p := t.TempDir()
	write(t, filepath.Join(p, "lib", "pkgconfig", "x.pc"), "prefix="+p)
	return p
}

func TestSeamStatError(t *testing.T) {
	defer restore()
	osStat = func(string) (os.FileInfo, error) { return nil, errInject }
	if err := FixUp(Options{Prefix: pcPrefix(t), Platform: "linux"}); !errors.Is(err, errInject) {
		t.Errorf("Stat seam: %v", err)
	}
}

func TestSeamWriteFileError(t *testing.T) {
	defer restore()
	osWriteFile = func(string, []byte, fs.FileMode) error { return errInject }
	if err := FixUp(Options{Prefix: pcPrefix(t), Platform: "linux"}); !errors.Is(err, errInject) {
		t.Errorf("WriteFile seam: %v", err)
	}
}

func TestSeamRemoveErrorInLa(t *testing.T) {
	defer restore()
	p := t.TempDir()
	write(t, filepath.Join(p, "lib", "z.la"), "x")
	osRemove = func(string) error { return errInject }
	if err := FixUp(Options{Prefix: p, Platform: "linux"}); !errors.Is(err, errInject) {
		t.Errorf("Remove(.la) seam: %v", err)
	}
}

// lib64Prefix has only lib64/ so the consolidate step is the first to touch
// osReadDir/osRename/osRemove/osSymlink.
func lib64Prefix(t *testing.T) string {
	p := t.TempDir()
	write(t, filepath.Join(p, "lib64", "libz.so"), "x")
	return p
}

func TestSeamConsolidateErrors(t *testing.T) {
	for name, setup := range map[string]func(){
		"readdir": func() { osReadDir = func(string) ([]os.DirEntry, error) { return nil, errInject } },
		"rename":  func() { osRename = func(string, string) error { return errInject } },
		"remove":  func() { osRemove = func(string) error { return errInject } },
		"symlink": func() { osSymlink = func(string, string) error { return errInject } },
	} {
		t.Run(name, func(t *testing.T) {
			defer restore()
			setup()
			if err := FixUp(Options{Prefix: lib64Prefix(t), Platform: "linux"}); !errors.Is(err, errInject) {
				t.Errorf("consolidate %s seam: %v", name, err)
			}
		})
	}
}

// incPrefix has only include/only/a.h so flatten is the first to touch these.
func incPrefix(t *testing.T) string {
	p := t.TempDir()
	write(t, filepath.Join(p, "include", "only", "a.h"), "h")
	return p
}

func TestSeamFlattenErrors(t *testing.T) {
	for name, setup := range map[string]func(){
		"subdir-readdir": func() {
			osReadDir = func(p string) ([]os.DirEntry, error) {
				if strings.HasSuffix(p, "only") {
					return nil, errInject
				}
				return os.ReadDir(p)
			}
		},
		"rename":  func() { osRename = func(string, string) error { return errInject } },
		"remove":  func() { osRemove = func(string) error { return errInject } },
		"symlink": func() { osSymlink = func(string, string) error { return errInject } },
	} {
		t.Run(name, func(t *testing.T) {
			defer restore()
			setup()
			if err := FixUp(Options{Prefix: incPrefix(t), Platform: "linux"}); !errors.Is(err, errInject) {
				t.Errorf("flatten %s seam: %v", name, err)
			}
		})
	}
}

func TestSeamSetRunpathWriteError(t *testing.T) {
	defer restore()
	// open read-only so the WriteAt inside SetRunpath fails after a clean parse
	osOpenFile = func(name string, _ int, _ fs.FileMode) (*os.File, error) {
		return os.OpenFile(name, os.O_RDONLY, 0)
	}
	p := buildELF64LE(t, "/opt/placeholder/aaaaaaaaaaaaaaaaaaaa", "libc.so.6", 40)
	if err := SetRunpath(p, "$ORIGIN/x"); err == nil {
		t.Error("expected WriteAt error via read-only handle")
	}
}

// The TAG write has its own error arm, and it is reached before the string
// write — so a read-only handle exercises it only with a DT_RPATH fixture.
// With a DT_RUNPATH one there is no tag to convert and the string write
// fails instead, which is what the test above covers.
func TestSeamSetRunpathTagWriteError(t *testing.T) {
	defer restore()
	osOpenFile = func(name string, _ int, _ fs.FileMode) (*os.File, error) {
		return os.OpenFile(name, os.O_RDONLY, 0)
	}
	p := buildELFTag(t, elf.DT_RPATH, "/opt/placeholder/aaaaaaaaaaaaaaaaaaaa", "libc.so.6", 40)
	if err := SetRunpath(p, "$ORIGIN/x"); err == nil {
		t.Error("expected the tag WriteAt to fail through a read-only handle")
	}
}

// dynTagBytes is two words wide at 64 bits and one at 32: writing eight
// bytes over a 32-bit d_tag would take the d_val with it.
func TestDynTagBytesIsClassWidth(t *testing.T) {
	if got := dynTagBytes(elf.ELFCLASS64, binary.LittleEndian, elf.DT_RUNPATH); len(got) != 8 ||
		binary.LittleEndian.Uint64(got) != uint64(elf.DT_RUNPATH) {
		t.Errorf("64-bit: %v", got)
	}
	if got := dynTagBytes(elf.ELFCLASS32, binary.BigEndian, elf.DT_RUNPATH); len(got) != 4 ||
		binary.BigEndian.Uint32(got) != uint32(elf.DT_RUNPATH) {
		t.Errorf("32-bit: %v", got)
	}
}
