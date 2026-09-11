package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bottle"
	"github.com/ulikunitz/xz"
)

// machoMagic are the four leading bytes of a Mach-O, thin or fat, either
// endianness. Matching on the bytes rather than on a path lets the scan skip
// the scripts, headers and data that make up most of a bottle.
var machoMagic = [][]byte{
	{0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}, // 32-bit
	{0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}, // 64-bit
	{0xca, 0xfe, 0xba, 0xbe}, {0xbe, 0xba, 0xfe, 0xca}, // fat
}

// seams (swapped in tests): these paths are filesystem failures — a temp
// directory that cannot be made, a file that cannot be written — which a test
// cannot provoke honestly and which are the only thing a user would ever see
// of them.
var (
	auditOpen      = os.Open
	auditMkdirTemp = os.MkdirTemp
	auditMkdirAll  = os.MkdirAll
	auditOpenFile  = os.OpenFile
)

func looksMachO(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	for _, m := range machoMagic {
		if bytes.Equal(b[:4], m) {
			return true
		}
	}
	return false
}

// auditMirroredBottle reports what FixUp's relocatability guards would have
// said about a bottle that was never unpacked.
//
// A mirrored bottle is republished verbatim, so `fixup` never runs on it and
// `ErrDeadRpath`/`ErrBuilderOnlyRpath` have no bytes to look at.
// `git-scm.org 2.55.0` reached the catalogue that way: 164 of its 164 Mach-O
// files reach a sibling package only through `/Users/runner/.pkgx`, so `git`
// cannot load zlib anywhere else. Nothing said so at publish time; it surfaced
// weeks later as a third package's build failure.
//
// It REPORTS. Whether a mirror must be relocatable is go-pkgx/packages#147's
// question; what a publisher should not have to do is learn it from someone
// else's dyld error.
//
// A bottle tarball's entries are already `<project>/v<ver>/…`, so extracting
// the Mach-O files alone into a temp directory reproduces the layout the guards
// need: that directory is $PKGX_DIR and the package prefix sits under it at its
// real depth, which is exactly what an @loader_path rpath is measured against.
func auditMirroredBottle(path, ext, proj, ver string) (checked int, problems []error, err error) {
	f, err := auditOpen(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	var r io.Reader
	switch ext {
	case bottle.ExtTarXz:
		if r, err = xz.NewReader(f); err != nil {
			return 0, nil, err
		}
	case bottle.ExtTarGz, "":
		g, gerr := gzip.NewReader(f)
		if gerr != nil {
			return 0, nil, gerr
		}
		defer g.Close()
		r = g
	default:
		// Say so rather than reporting a clean bill: an audit that could not
		// read must not read as an audit that found nothing.
		return 0, nil, fmt.Errorf("cannot audit %s: unhandled compression %q", path, ext)
	}

	dir, err := auditMkdirTemp("", "bk-mirror-audit-")
	if err != nil {
		return 0, nil, err
	}
	defer os.RemoveAll(dir)

	tr := tar.NewReader(r)
	for {
		h, nerr := tr.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return 0, nil, nerr
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		head := make([]byte, 4)
		n, _ := io.ReadFull(tr, head)
		if !looksMachO(head[:n]) {
			continue
		}
		rel := filepath.Clean(filepath.FromSlash(h.Name))
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue // a tarball naming its way out of the tree audits nothing
		}
		dst := filepath.Join(dir, rel)
		if err := auditMkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, nil, err
		}
		out, cerr := auditOpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if cerr != nil {
			return 0, nil, cerr
		}
		_, werr := out.Write(head[:n])
		if werr == nil {
			_, werr = ioCopy(out, tr)
		}
		out.Close()
		if werr != nil {
			return 0, nil, werr
		}
	}
	prefix := filepath.Join(dir, filepath.FromSlash(proj), "v"+ver)
	checked, problems = fixup.AuditRelocatable(prefix, dir)
	return checked, problems, nil
}
