package fixup

import (
	"debug/macho"
	"encoding/binary"
	"errors"
	"strings"
)

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

// isMachO reports whether path is a thin Mach-O object (fat binaries are
// skipped: pkgx builds are per-arch/thin).
func isMachO(path string) bool {
	f, err := macho.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// machoInfo opens a Mach-O and returns its raw bytes, byte order, header size
// and load-command count.
func machoInfo(path string) (raw []byte, bo binary.ByteOrder, hdr, ncmd int, err error) {
	f, err := macho.Open(path)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	bo = f.ByteOrder
	hdr = 28
	if f.Magic == macho.Magic64 {
		hdr = 32
	}
	ncmd = int(f.Ncmd)
	f.Close()
	raw, err = osReadFile(path)
	return raw, bo, hdr, ncmd, err
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
// Mach-O (LC_ID_DYLIB, LC_LOAD_DYLIB and friends, LC_RPATH).
func ReadMachoStrings(path string) ([]string, error) {
	raw, bo, hdr, ncmd, err := machoInfo(path)
	if err != nil {
		return nil, err
	}
	var out []string
	walkMachoStrings(raw, bo, hdr, ncmd, func(_ uint32, s string) string {
		out = append(out, s)
		return s
	})
	return out, nil
}

// RewriteMachoStrings rewrites every dylib-name/rpath string in a thin Mach-O in
// place by applying fn. It cannot grow a string (Mach-O load commands are a
// fixed size), so a longer replacement returns ErrNoSpace; a shorter one is
// zero-padded. Stripping a staging suffix (…+brewing) always shrinks, so fits.
func RewriteMachoStrings(path string, fn func(string) string) error {
	raw, bo, hdr, ncmd, err := machoInfo(path)
	if err != nil {
		return err
	}
	changed, err := walkMachoStrings(raw, bo, hdr, ncmd, func(_ uint32, s string) string { return fn(s) })
	if err != nil {
		return err
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

// rewriteMacho strips the +brewing staging prefix from a Mach-O's install name
// and dep references (buildInstall → prefix), the darwin analogue of the .pc /
// .cmake path rewrite. Removing +brewing shrinks the string, so it always fits.
func rewriteMacho(exe string, opts Options) error {
	if opts.BuildInstall == "" || !isMachO(exe) {
		return nil
	}
	err := RewriteMachoStrings(exe, func(s string) string {
		return strings.ReplaceAll(s, opts.BuildInstall, opts.Prefix)
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
