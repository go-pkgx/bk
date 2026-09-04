package fixup

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"hash"
)

// Mach-O code signing, enough of it to re-sign what we just rewrote.
//
// Editing a Mach-O in place invalidates its signature, and on Apple silicon
// that is FATAL and SILENT — the kernel kills the process before it prints
// anything:
//
//	$ ./bin/main
//	$ echo $?
//	137                                     ← SIGKILL, no message at all
//	$ codesign -v bin/main
//	bin/main: invalid signature (code or signature have been modified)
//
// The linker ad-hoc signs every arm64 binary it produces (flags 0x20002 =
// adhoc|linker-signed), so this applies to essentially everything bk builds
// for darwin/aarch64. Stripping +brewing from an install name is exactly such
// an edit. Without what follows, a bottle whose paths we fixed would be a
// bottle nobody can run, and the failure would look like nothing at all.
//
// Re-signing is affordable here because our edits are SIZE-PRESERVING: strings
// are overwritten in their existing load-command slot and zero-padded. The
// signature blob stays where it is, the code limit does not move, the number
// of page hashes does not change. Only the digests themselves are stale — so
// re-signing is recomputing them in place, which needs no key, no CMS, and no
// external tool. `codesign -s -` would do the same thing by shelling out to a
// program bk cannot rely on when cross-building.
//
// The layout below was verified against a real published bottle before any of
// it was used: recomputing every code slot of ghcr.io/go-pkgx/packages'
// darwin/aarch64 `rust-lang.org/cargo` v0.99.0 reproduced 8159 of 8159 stored
// hashes. An unmodified binary hashing to itself is the proof the structure is
// read correctly.
const (
	lcCodeSignature = 0x1d

	csMagicEmbeddedSignature = 0xfade0cc0
	csMagicCodeDirectory     = 0xfade0c02

	// Hash algorithms a CodeDirectory can name (cs_blobs.h CS_HASHTYPE_*).
	csHashSHA1            = 1
	csHashSHA256          = 2
	csHashSHA256Truncated = 3
	csHashSHA384          = 4
)

// ErrBadSignature means the embedded signature is not shaped the way the
// format says. bk refuses to guess at it: a half-understood signature rewritten
// anyway produces a binary that dies with no message.
var ErrBadSignature = errors.New("fixup: malformed Mach-O code signature")

// resignSlice recomputes the code-slot hashes of the embedded signature in one
// Mach-O slice, in place. slice is the whole thin image (offsets inside a
// signature are relative to it, not to the containing fat file).
//
// It reports whether a signature was found and rewritten; an unsigned slice is
// not an error, it simply has nothing to restate.
func resignSlice(slice []byte, bo binary.ByteOrder, hdr, ncmd int) (bool, error) {
	off, size, ok := codeSignatureCmd(slice, bo, hdr, ncmd)
	if !ok {
		return false, nil
	}
	if off+size > len(slice) {
		return false, ErrBadSignature
	}
	return resignSuperBlob(slice, slice[off:off+size])
}

// codeSignatureCmd finds LC_CODE_SIGNATURE and returns the file offset and
// length of the signature blob it points at.
func codeSignatureCmd(slice []byte, bo binary.ByteOrder, hdr, ncmd int) (off, size int, ok bool) {
	p := hdr
	for i := 0; i < ncmd && p+8 <= len(slice); i++ {
		cmd := bo.Uint32(slice[p:])
		sz := int(bo.Uint32(slice[p+4:]))
		if sz < 8 || p+sz > len(slice) {
			return 0, 0, false
		}
		if cmd == lcCodeSignature && sz >= 16 {
			return int(bo.Uint32(slice[p+8:])), int(bo.Uint32(slice[p+12:])), true
		}
		p += sz
	}
	return 0, 0, false
}

// resignSuperBlob walks an embedded signature SuperBlob and recomputes every
// CodeDirectory it holds. There can be more than one — a binary signed for
// several hash algorithms carries an "alternate" directory per algorithm, and
// leaving one of them stale is the same failure as leaving them all stale.
//
// Everything in a signature blob is BIG-endian, whatever the Mach-O's own byte
// order: it is a cross-architecture structure.
func resignSuperBlob(slice, sig []byte) (bool, error) {
	be := binary.BigEndian
	if len(sig) < 12 || be.Uint32(sig) != csMagicEmbeddedSignature {
		// Not an embedded signature (a detached or unknown blob): leave it be
		// rather than write over something we do not understand.
		return false, nil
	}
	count := int(be.Uint32(sig[8:]))
	if 12+count*8 > len(sig) {
		return false, ErrBadSignature
	}
	done := false
	for i := 0; i < count; i++ {
		o := int(be.Uint32(sig[12+i*8+4:]))
		if o < 0 || o+8 > len(sig) {
			return done, ErrBadSignature
		}
		if be.Uint32(sig[o:]) != csMagicCodeDirectory {
			continue
		}
		length := int(be.Uint32(sig[o+4:]))
		if length < 44 || o+length > len(sig) {
			return done, ErrBadSignature
		}
		if err := resignCodeDirectory(slice, sig[o:o+length]); err != nil {
			return done, err
		}
		done = true
	}
	return done, nil
}

// resignCodeDirectory rewrites the code slots of one CodeDirectory.
//
// Special slots (Info.plist, requirements, entitlements…) live at NEGATIVE
// indices, before hashOffset, and hash blobs we never touch — so they stay as
// they are. Only the code slots, which hash the image itself, went stale.
func resignCodeDirectory(slice, cd []byte) error {
	be := binary.BigEndian
	var (
		version   = be.Uint32(cd[8:])
		hashOff   = int(be.Uint32(cd[16:]))
		nCode     = int(be.Uint32(cd[28:]))
		codeLimit = int64(be.Uint32(cd[32:]))
		hashSize  = int(cd[36])
		hashType  = cd[37]
		pageShift = cd[39]
	)
	// A >4GiB image cannot state its limit in the uint32 field; version 0x20300
	// added a 64-bit one at offset 56 and leaves the old field zero.
	if version >= 0x20300 && codeLimit == 0 && len(cd) >= 64 {
		codeLimit = int64(be.Uint64(cd[56:]))
	}
	newHash, digestSize := hasherFor(hashType)
	if newHash == nil {
		return errors.New("fixup: unknown code signature hash type")
	}
	if hashSize <= 0 || hashSize > digestSize {
		return ErrBadSignature
	}
	if codeLimit < 0 || codeLimit > int64(len(slice)) {
		return ErrBadSignature
	}
	if hashOff < 0 || hashOff+nCode*hashSize > len(cd) {
		return ErrBadSignature
	}
	// pageSize 0 means the whole image is hashed as one slot rather than paged.
	page := int64(1) << pageShift
	if pageShift == 0 {
		page = codeLimit
	}
	for i := 0; i < nCode; i++ {
		start := int64(i) * page
		if start > codeLimit {
			return ErrBadSignature
		}
		end := start + page
		if end > codeLimit {
			end = codeLimit
		}
		h := newHash()
		h.Write(slice[start:end])
		copy(cd[hashOff+i*hashSize:hashOff+(i+1)*hashSize], h.Sum(nil)[:hashSize])
	}
	return nil
}

// hasherFor maps a CS_HASHTYPE_* to its constructor and full digest length.
// The truncated variant is the same digest cut to the directory's hashSize,
// which the caller applies, so it shares SHA-256's constructor.
func hasherFor(t byte) (func() hash.Hash, int) {
	switch t {
	case csHashSHA1:
		return func() hash.Hash { return sha1.New() }, sha1.Size
	case csHashSHA256, csHashSHA256Truncated:
		return func() hash.Hash { return sha256.New() }, sha256.Size
	case csHashSHA384:
		return func() hash.Hash { return sha512.New384() }, sha512.Size384
	}
	return nil, 0
}
