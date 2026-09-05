package fixup

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
)

// fatMagic64 is the 64-bit universal-binary magic (FAT_MAGIC_64, big-endian on
// disk); debug/macho only knows the 32-bit macho.MagicFat (0xcafebabe).
const fatMagic64 = 0xcafebabf

// Mach-O load commands that carry a path/name string at struct offset 8
// (DylibCmd.Name and RpathCmd.Path are both the third uint32).
const (
	lcReqDyld       = 0x80000000
	lcLoadDylib     = 0xc
	lcIDDylib       = 0xd
	lcLoadWeakDylib = 0x18 | lcReqDyld
	lcReexportDylib = 0x1f | lcReqDyld
	lcRpath         = 0x1c | lcReqDyld
)

func machoStringCmd(cmd uint32) bool {
	switch cmd {
	case lcLoadDylib, lcIDDylib, lcLoadWeakDylib, lcReexportDylib, lcRpath:
		return true
	}
	return false
}

// isMachO reports whether path is a Mach-O object, thin or fat (universal).
func isMachO(path string) bool {
	_, _, err := machoInfo(path)
	return err == nil
}

// machoSlice locates one architecture slice inside a Mach-O file (a thin file
// is a single slice spanning the whole file): byte range, byte order, header
// size and load-command count.
type machoSlice struct {
	off, size int
	bo        binary.ByteOrder
	hdr, ncmd int
}

// machoInfo reads a Mach-O (thin or fat) and returns its raw bytes plus the
// parsed header info of every architecture slice.
func machoInfo(path string) ([]byte, []machoSlice, error) {
	raw, err := osReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	slices, err := machoSlices(raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, slices, nil
}

// machoSlices parses raw as a thin or fat (universal) Mach-O. A fat file
// starts with FAT_MAGIC/FAT_MAGIC_64 and nfat_arch (both uint32 BE) followed
// by nfat_arch fat_arch entries; each entry locates one thin slice.
func machoSlices(raw []byte) ([]machoSlice, error) {
	if len(raw) >= 8 {
		switch binary.BigEndian.Uint32(raw) {
		case macho.MagicFat:
			ff, err := macho.NewFatFile(bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			var out []machoSlice
			for _, a := range ff.Arches {
				hdr := 28
				if a.Magic == macho.Magic64 {
					hdr = 32
				}
				out = append(out, machoSlice{off: int(a.Offset), size: int(a.Size), bo: a.ByteOrder, hdr: hdr, ncmd: int(a.Ncmd)})
			}
			return out, nil
		case fatMagic64:
			return fat64Slices(raw)
		}
	}
	sl, err := thinSlice(raw, 0, len(raw))
	if err != nil {
		return nil, err
	}
	return []machoSlice{sl}, nil
}

// fat64Slices hand-parses a FAT_MAGIC_64 header (debug/macho only supports the
// 32-bit fat format): nfat_arch fat_arch_64 entries of 32 bytes — cputype(4)
// cpusubtype(4) offset(8) size(8) align(4) reserved(4) — offset and size as
// uint64 BE at entry offsets 8 and 16.
func fat64Slices(raw []byte) ([]machoSlice, error) {
	be := binary.BigEndian
	narch := int(be.Uint32(raw[4:]))
	var out []machoSlice
	for i := 0; i < narch; i++ {
		e := 8 + i*32
		if e+32 > len(raw) {
			return nil, errors.New("fixup: truncated fat_arch_64 table")
		}
		sl, err := thinSlice(raw, int(be.Uint64(raw[e+8:])), int(be.Uint64(raw[e+16:])))
		if err != nil {
			return nil, err
		}
		out = append(out, sl)
	}
	return out, nil
}

// thinSlice parses the thin Mach-O header at raw[off:off+size].
func thinSlice(raw []byte, off, size int) (machoSlice, error) {
	if off < 0 || off > len(raw) || size < 0 || size > len(raw)-off {
		return machoSlice{}, errors.New("fixup: Mach-O slice out of bounds")
	}
	f, err := macho.NewFile(bytes.NewReader(raw[off : off+size]))
	if err != nil {
		return machoSlice{}, err
	}
	hdr := 28
	if f.Magic == macho.Magic64 {
		hdr = 32
	}
	sl := machoSlice{off: off, size: size, bo: f.ByteOrder, hdr: hdr, ncmd: int(f.Ncmd)}
	f.Close()
	return sl, nil
}

// walkMachoStrings visits each dylib-name/rpath string, replacing it in raw with
// visit's return value (in place). It reports whether anything changed and
// returns ErrNoSpace if a replacement is longer than its fixed load-command slot.
func walkMachoStrings(raw []byte, bo binary.ByteOrder, hdr, ncmd int, visit func(cmd uint32, s string) string) (bool, error) {
	changed := false
	off := hdr
	for i := 0; i < ncmd && off+8 <= len(raw); i++ {
		cmd := bo.Uint32(raw[off:])
		size := int(bo.Uint32(raw[off+4:]))
		if size < 8 || off+size > len(raw) {
			break
		}
		if machoStringCmd(cmd) {
			strOff := int(bo.Uint32(raw[off+8:]))
			start, end := off+strOff, off+size
			if strOff >= 8 && start <= end {
				old := cstr(raw[start:end])
				if nw := visit(cmd, old); nw != old {
					if len(nw) >= end-start {
						return changed, ErrNoSpace
					}
					for j := start; j < end; j++ {
						raw[j] = 0
					}
					copy(raw[start:], nw)
					changed = true
				}
			}
		}
		off += size
	}
	return changed, nil
}

// ReadMachoStrings returns the install name, dylib references and rpaths of a
// Mach-O (LC_ID_DYLIB, LC_LOAD_DYLIB and friends, LC_RPATH), from every
// architecture slice of a fat binary.
func ReadMachoStrings(path string) ([]string, error) {
	raw, slices, err := machoInfo(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sl := range slices {
		walkMachoStrings(raw[sl.off:sl.off+sl.size], sl.bo, sl.hdr, sl.ncmd, func(_ uint32, s string) string {
			out = append(out, s)
			return s
		})
	}
	return out, nil
}

// RewriteMachoStrings rewrites every dylib-name/rpath string in a Mach-O in
// place by applying fn — every architecture slice of a fat binary. It cannot
// grow a string (Mach-O load commands are a fixed size), so a longer
// replacement returns ErrNoSpace; a shorter one is zero-padded. Stripping a
// staging suffix (…+brewing) always shrinks, so fits.
//
// Any slice it changes is re-signed (see machosign.go): an edited Mach-O whose
// signature still describes the old bytes is not a binary with a stale
// signature, it is a binary that cannot run at all.
func RewriteMachoStrings(path string, fn func(string) string) error {
	return rewriteMachoStringsCmd(path, func(_ uint32, s string) string { return fn(s) })
}

// rewriteMachoStringsCmd is RewriteMachoStrings with the load command in hand:
// an rpath and an install name are both strings in the same kind of slot, and
// what each may be rewritten to is not the same.
func rewriteMachoStringsCmd(path string, fn func(uint32, string) string) error {
	raw, slices, err := machoInfo(path)
	if err != nil {
		return err
	}
	changed := false
	for _, sl := range slices {
		slice := raw[sl.off : sl.off+sl.size]
		ch, err := walkMachoStrings(slice, sl.bo, sl.hdr, sl.ncmd, fn)
		changed = changed || ch
		if err != nil {
			return err
		}
		// A rewritten slice no longer matches its own signature, and on Apple
		// silicon the kernel kills such a binary outright — with no message, no
		// dyld error, just exit 137. Restate the hashes over what we just wrote.
		if ch {
			if _, err := resignSlice(slice, sl.bo, sl.hdr, sl.ncmd); err != nil {
				return err
			}
		}
	}
	if !changed {
		return nil
	}
	fi, err := osStat(path)
	if err != nil {
		return err
	}
	return osWriteFile(path, raw, fi.Mode().Perm())
}

// machoRpaths returns a Mach-O's LC_RPATH entries.
func machoRpaths(path string) ([]string, error) {
	raw, slices, err := machoInfo(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sl := range slices {
		walkMachoStrings(raw[sl.off:sl.off+sl.size], sl.bo, sl.hdr, sl.ncmd, func(cmd uint32, s string) string {
			if cmd == lcRpath {
				out = append(out, s)
			}
			return s
		})
	}
	return out, nil
}

// rpathReaches reports whether any of a Mach-O's rpaths resolves to dir once
// the binary sits at exe. @loader_path is resolved against the file's own
// directory — which, fixup running after the staging tree has been renamed
// into place, is where it will actually be.
func rpathReaches(exe, dir string, rpaths []string) bool {
	want := filepath.Clean(dir)
	for _, r := range rpaths {
		var got string
		switch {
		case strings.HasPrefix(r, "@loader_path/"), strings.HasPrefix(r, "@executable_path/"):
			_, rel, _ := strings.Cut(r, "/")
			got = filepath.Join(filepath.Dir(exe), rel)
		case filepath.IsAbs(r):
			got = filepath.Clean(r)
		default:
			continue
		}
		if got == want {
			return true
		}
	}
	return false
}

// underDir reports whether p names something inside dir, and what it is called
// there. A prefix match alone would accept "/x/pkgxdirty" for "/x/pkgxdir".
func underDir(p, dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	dir = filepath.Clean(dir) + string(filepath.Separator)
	if !strings.HasPrefix(p, dir) || len(p) == len(dir) {
		return "", false
	}
	return filepath.ToSlash(p[len(dir):]), true
}

// rewriteMacho strips the +brewing staging prefix from a Mach-O's install name
// and dep references (buildInstall → prefix), the darwin analogue of the .pc /
// .cmake path rewrite. Removing +brewing shrinks the string, so it always fits.
func rewriteMacho(exe string, opts Options) error {
	if !isMachO(exe) {
		return nil
	}
	// A darwin binary names its dependencies by ABSOLUTE install name — the
	// LC_ID_DYLIB the dependency was built with, which is a path on the machine
	// that built it:
	//
	//   Library not loaded: /Users/runner/.pkgx/libssh2.org/v1.11.1/lib/libssh2.1.dylib
	//
	// That is what makes a darwin bottle unusable anywhere but its build
	// machine, and no rpath rescues it: an absolute reference never consults
	// one. Rewriting those into @rpath/… is what makes the bottle relocatable.
	//
	// But ONLY if this file carries an rpath that reaches $PKGX_DIR. Turning a
	// reference that works on the build machine into an @rpath one that
	// resolves nowhere would trade a bottle that works in one place for a
	// bottle that works in none — so it is measured per file, not assumed.
	toRpath := false
	if opts.PkgxDir != "" {
		rpaths, err := machoRpaths(exe)
		if err != nil {
			return err
		}
		toRpath = rpathReaches(exe, opts.PkgxDir, rpaths)
		if !toRpath {
			opts.log("macho %s: no rpath reaching %s, leaving install names absolute", exe, opts.PkgxDir)
		}
	}
	if opts.BuildInstall == "" && !toRpath {
		return nil
	}
	err := rewriteMachoStringsCmd(exe, func(cmd uint32, s string) string {
		if opts.BuildInstall != "" {
			s = strings.ReplaceAll(s, opts.BuildInstall, opts.Prefix)
		}
		// An rpath is a search ROOT, not a reference: @rpath means nothing
		// inside one, and the relative entries were linked in already.
		if !toRpath || cmd == lcRpath {
			return s
		}
		// A reference that is ALREADY @rpath/… — because the dependency was
		// itself built with an @rpath install name — never passed through the
		// absolute-path branch below, so its version stayed whole. Measured on
		// the grep bottle rebuilt once pcre2 had been: it recorded
		// @rpath/pcre.org/v2/v10.48/lib/libpcre2-8.0.dylib, which breaks again
		// the day pcre2 reaches 10.49. The relocation was right and the version
		// was not.
		if rest, ok := strings.CutPrefix(s, "@rpath/"); ok && cmd != lcRpath {
			full := filepath.Join(opts.PkgxDir, rest)
			t := transformRpath(full, filepath.Dir(opts.Prefix))
			if short, ok := underDir(t, opts.PkgxDir); ok {
				return "@rpath/" + short
			}
			return s
		}
		if _, ok := underDir(s, opts.PkgxDir); ok {
			// Major-versioned, exactly as the ELF side does to its RUNPATH
			// entries, and for the reason measured on the published grep bottle:
			// it named
			//
			//   /Users/runner/.pkgx/pcre.org/v2/v10.47/lib/libpcre2-8.0.dylib
			//
			// and the machine had pcre2 v10.48. grep could not start, so curl's
			// configure reported "'grep' utility not found in 'PATH'" — a
			// dependency's MINOR upgrade orphaning its dependents, reported as
			// a missing tool.
			//
			// pkgx installs each version in its own directory AND links the
			// major (v1 -> v1.3.2, v3 -> v3.6.4 — measured in an installed
			// tree), so v10 resolves to whatever 10.x is there. A package's own
			// libraries keep their full version: they ship together and cannot
			// disagree.
			t := transformRpath(s, filepath.Dir(opts.Prefix))
			rest, _ := underDir(t, opts.PkgxDir)
			return "@rpath/" + rest
		}
		return s
	})
	if errors.Is(err, ErrNoSpace) {
		opts.log("skip macho for %s: %v", exe, err)
		return nil
	}
	return err
}

// cstr reads a NUL-terminated string from the front of b.
func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
