package fixup

import (
	"debug/elf"
	"errors"
	"testing"
)

func TestRelToError(t *testing.T) {
	// filepath.Rel errors when one path is absolute and the other relative;
	// relTo then returns the target unchanged.
	if got := relTo("/abs/target", "relative-base"); got != "/abs/target" {
		t.Errorf("relTo error-path = %q", got)
	}
}

func TestOriginRelError(t *testing.T) {
	// a relative exeDir with an absolute target makes filepath.Rel fail;
	// originRel returns the input unchanged.
	if got := originRel("/abs/lib", "relative-dir"); got != "/abs/lib" {
		t.Errorf("originRel error-path = %q", got)
	}
}

func TestReadRunpathRPathOnly(t *testing.T) {
	// an ELF carrying only DT_RPATH (no DT_RUNPATH) still resolves.
	p := buildELFTag(t, elf.DT_RPATH, "/opt/rpath/only", "libc.so.6", 32)
	rp, err := ReadRunpath(p)
	if err != nil || rp != "/opt/rpath/only" {
		t.Fatalf("ReadRunpath rpath-only = %q err=%v", rp, err)
	}
}

func TestReadRunpathNone(t *testing.T) {
	// a dynamic ELF with NO rpath entry: ReadRunpath is empty, SetRunpath says so.
	p := buildELFTag(t, elf.DT_NULL, "", "libc.so.6", 0)
	rp, err := ReadRunpath(p)
	if err != nil || rp != "" {
		t.Fatalf("ReadRunpath none = %q err=%v", rp, err)
	}
	if !isDynamicELF(p) {
		t.Error("still a dynamic ELF")
	}
	if err := SetRunpath(p, "$ORIGIN/x"); !errors.Is(err, ErrNoRunpath) {
		t.Fatalf("SetRunpath none = %v, want ErrNoRunpath", err)
	}
}
