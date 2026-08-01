package fixup

import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Sentinels returned by SetRunpath.
var (
	// ErrNoRunpath means the ELF has neither DT_RUNPATH nor DT_RPATH, so there
	// is no string slot to overwrite in place.
	ErrNoRunpath = errors.New("fixup: ELF has no DT_RUNPATH/DT_RPATH entry")
	// ErrNoSpace means the new RUNPATH is longer than the existing string slot.
	// A pure-Go in-place rewrite cannot grow .dynstr; link the binary with a
	// longer -Wl,-rpath placeholder so the final value fits.
	ErrNoSpace = errors.New("fixup: new RUNPATH longer than existing slot")
)

// ReadRunpath returns the DT_RUNPATH (preferred) or DT_RPATH of an ELF, or ""
// if it has neither.
func ReadRunpath(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if v, err := f.DynString(elf.DT_RUNPATH); err == nil && len(v) > 0 {
		return v[0], nil
	}
	if v, err := f.DynString(elf.DT_RPATH); err == nil && len(v) > 0 {
		return v[0], nil
	}
	return "", nil
}

// ReadNeeded returns the DT_NEEDED shared-library names of an ELF.
func ReadNeeded(path string) ([]string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.DynString(elf.DT_NEEDED)
}

// isDynamicELF reports whether path is a dynamically-linked ELF (has a
// .dynamic section) that carries a symbol table worth relocating.
func isDynamicELF(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return f.Section(".dynamic") != nil && f.Section(".dynstr") != nil
}

// SetRunpath overwrites an ELF's existing DT_RUNPATH/DT_RPATH string in place.
// It is pure-Go (no patchelf): it locates the string offset in .dynstr and
// overwrites the bytes, zero-padding any slack. It cannot grow .dynstr, so it
// returns ErrNoSpace when value is longer than the current slot and ErrNoRunpath
// when there is no rpath entry at all.
func SetRunpath(path, value string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	class, order := f.Class, f.ByteOrder
	dynSec := f.Section(".dynamic")
	strSec := f.Section(".dynstr")
	if dynSec == nil || strSec == nil {
		f.Close()
		return ErrNoRunpath
	}
	dyn, err := dynSec.Data()
	if err != nil {
		f.Close()
		return err
	}
	f.Close()

	strOff, ok := runpathStrOffset(dyn, class, order)
	if !ok {
		return ErrNoRunpath
	}

	fh, err := osOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fh.Close()

	// measure the existing slot: bytes from the string start to its NUL.
	base := int64(strSec.Offset) + int64(strOff)
	slot := make([]byte, 4096)
	n, _ := fh.ReadAt(slot, base)
	oldLen := 0
	for oldLen < n && slot[oldLen] != 0 {
		oldLen++
	}
	if len(value) > oldLen {
		return ErrNoSpace
	}
	buf := make([]byte, oldLen+1) // value + NUL, zero-padded to the old length
	copy(buf, value)
	if _, err := fh.WriteAt(buf, base); err != nil {
		return err
	}
	return nil
}

// runpathStrOffset scans a raw .dynamic section for DT_RUNPATH (preferred) or
// DT_RPATH and returns its d_val (a .dynstr byte offset).
func runpathStrOffset(dyn []byte, class elf.Class, order binary.ByteOrder) (uint64, bool) {
	var rpath uint64
	haveRPath := false
	step := 16
	if class == elf.ELFCLASS32 {
		step = 8
	}
	for i := 0; i+step <= len(dyn); i += step {
		var tag int64
		var val uint64
		if class == elf.ELFCLASS64 {
			tag = int64(order.Uint64(dyn[i:]))
			val = order.Uint64(dyn[i+8:])
		} else {
			tag = int64(int32(order.Uint32(dyn[i:])))
			val = uint64(order.Uint32(dyn[i+4:]))
		}
		switch elf.DynTag(tag) {
		case elf.DT_RUNPATH:
			return val, true // RUNPATH wins outright
		case elf.DT_RPATH:
			rpath, haveRPath = val, true
		case elf.DT_NULL:
			// end of the dynamic array
			if haveRPath {
				return rpath, true
			}
			return 0, false
		}
	}
	if haveRPath {
		return rpath, true
	}
	return 0, false
}

// fixRpaths walks bin/lib/libexec and rewrites each dynamic ELF's RUNPATH to
// $ORIGIN-relative references to the package's own libs and its deps.
func fixRpaths(opts Options) error {
	switch opts.Platform {
	case "linux":
		if has(opts.Skips, "fix-patchelf") {
			opts.log("skipping rpath fixes for %s", opts.Prefix)
			return nil
		}
		return walkExes(opts.Prefix, func(exe string) error {
			return rewriteRunpath(exe, opts)
		})
	case "darwin":
		if has(opts.Skips, "fix-machos") {
			opts.log("skipping mach-o fixes for %s", opts.Prefix)
			return nil
		}
		// Mach-O install-name/rpath rewriting is not yet implemented in pure Go
		// (brewkit shells out to fix-machos.rb). Documented gap — bk targets
		// linux + windows cross first.
		opts.log("note: darwin mach-o rpath fix-up not yet implemented for %s", opts.Prefix)
		return nil
	default:
		return nil
	}
}

func rewriteRunpath(exe string, opts Options) error {
	if !isDynamicELF(exe) {
		return nil
	}
	exeDir := filepath.Dir(exe)
	projectDir := filepath.Dir(opts.Prefix) // …/project (parent of vX.Y.Z)

	var rpaths []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			rpaths = append(rpaths, s)
		}
	}
	// After relocation every RUNPATH entry must be $ORIGIN-relative. Keep the
	// build's own $ORIGIN entries and any pointing into this package's project
	// dir (rewritten $ORIGIN-relative); DROP foreign absolute entries — a
	// build-time path or the link-time placeholder is meaningless at runtime
	// and would blow the in-place slot budget.
	if cur, err := ReadRunpath(exe); err == nil && cur != "" {
		for _, p := range strings.Split(cur, ":") {
			switch {
			case strings.HasPrefix(p, "$ORIGIN"):
				add(p)
			case p != "" && strings.HasPrefix(p, projectDir):
				add(originRel(p, exeDir))
			}
		}
	}
	for _, dep := range opts.DepPaths {
		add(originRel(transformRpath(dep, projectDir), exeDir))
	}
	if len(rpaths) == 0 {
		return nil
	}
	err := SetRunpath(exe, strings.Join(rpaths, ":"))
	if errors.Is(err, ErrNoRunpath) || errors.Is(err, ErrNoSpace) {
		// tolerant, like patchelf on statically-linked/oddball inputs
		opts.log("skip rpath for %s: %v", exe, err)
		return nil
	}
	return err
}

var versionRE = regexp.MustCompile(`v(\d+)\.(\d+\.)+\d+[a-z]?`)

// transformRpath major-versions a full version path so bottles survive minor
// upgrades without a re-sign; $ORIGIN paths and references into the package's
// own project dir are left untouched.
func transformRpath(input, projectDir string) string {
	if strings.HasPrefix(input, "$ORIGIN") {
		return input
	}
	if strings.HasPrefix(input, projectDir) {
		return input
	}
	return versionRE.ReplaceAllString(input, "v$1")
}

// originRel makes an absolute path $ORIGIN-relative to exeDir; $ORIGIN paths
// pass through unchanged.
func originRel(p, exeDir string) string {
	if strings.HasPrefix(p, "$ORIGIN") {
		return p
	}
	rel, err := filepath.Rel(exeDir, p)
	if err != nil {
		return p
	}
	return "$ORIGIN/" + filepath.ToSlash(rel)
}

// walkExes visits regular (non-symlink) files under bin/lib/libexec.
func walkExes(prefix string, fn func(string) error) error {
	for _, sub := range []string{"bin", "lib", "libexec"} {
		d := filepath.Join(prefix, sub)
		if !isDir(d) {
			continue
		}
		err := filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !e.Type().IsRegular() {
				return nil
			}
			switch filepath.Ext(p) {
			case ".py", ".pyc", ".pl", ".h", ".a", ".la", ".pc":
				return nil
			}
			return fn(p)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
