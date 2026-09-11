package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bottle"
)

// machoBytes builds a minimal thin Mach-O64 (LE) carrying the given load
// commands. cmd 0xc is LC_LOAD_DYLIB (string at offset 24), 0x8000001c is
// LC_RPATH (string at offset 12).
func machoBytes(cmds ...[2]any) []byte {
	le := binary.LittleEndian
	var body []byte
	for _, c := range cmds {
		cmd, str := c[0].(uint32), c[1].(string)
		strOff := 24
		if cmd == 0x8000001c {
			strOff = 12
		}
		size := strOff + len(str) + 1
		for size%8 != 0 {
			size++
		}
		cb := make([]byte, size)
		le.PutUint32(cb[0:], cmd)
		le.PutUint32(cb[4:], uint32(size))
		le.PutUint32(cb[8:], uint32(strOff))
		copy(cb[strOff:], str)
		body = append(body, cb...)
	}
	buf := make([]byte, 32+len(body))
	le.PutUint32(buf[0:], 0xfeedfacf)
	le.PutUint32(buf[4:], 0x0100000c)
	le.PutUint32(buf[12:], 6)
	le.PutUint32(buf[16:], uint32(len(cmds)))
	le.PutUint32(buf[20:], uint32(len(body)))
	copy(buf[32:], body)
	return buf
}

// gzBottle writes a .tar.gz whose entries are name->content, as a bottle
// tarball is laid out: `<project>/v<version>/…`.
func gzBottle(t *testing.T, files map[string][]byte) string {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "v2.55.0.tar.gz")
	if err := os.WriteFile(p, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	lcLoad  = uint32(0xc)
	lcRpath = uint32(0x8000001c)
)

// TestAuditMirroredBottleFindsWhatFixupNeverSaw reproduces git-scm.org 2.55.0's
// shape: a qualified @rpath reference to a sibling package, and one rpath —
// the build machine's own absolute path. A mirror is never unpacked, so no
// guard has ever seen it.
func TestAuditMirroredBottleFindsWhatFixupNeverSaw(t *testing.T) {
	tb := gzBottle(t, map[string][]byte{
		"git-scm.org/v2.55.0/libexec/git": machoBytes(
			[2]any{lcLoad, "@rpath/zlib.net/v1.3.2/lib/libz.1.dylib"},
			[2]any{lcRpath, "/Users/runner/.pkgx"}),
		"git-scm.org/v2.55.0/lib/libok.dylib": machoBytes(
			[2]any{lcLoad, "@rpath/zlib.net/v1.3.2/lib/libz.1.dylib"},
			[2]any{lcRpath, "@loader_path/../../.."}),
		// Most of a bottle is not Mach-O and must cost nothing.
		"git-scm.org/v2.55.0/bin/git":              []byte("#!/bin/sh\nexec true\n"),
		"git-scm.org/v2.55.0/share/man/man1/git.1": []byte(".TH GIT 1\n"),
	})
	checked, problems, err := auditMirroredBottle(tb, bottle.ExtTarGz, "git-scm.org", "2.55.0")
	if err != nil {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Fatalf("checked %d Mach-O, want 2 (the shell script and the man page are not)", checked)
	}
	if len(problems) != 1 {
		t.Fatalf("reported %d problem(s), want 1: %v", len(problems), problems)
	}
	if !errors.Is(problems[0], fixup.ErrBuilderOnlyRpath) {
		t.Errorf("problem is %v, want ErrBuilderOnlyRpath", problems[0])
	}
	if !strings.Contains(problems[0].Error(), filepath.Join("libexec", "git")) {
		t.Errorf("problem does not name libexec/git: %v", problems[0])
	}
}

// TestAuditMirroredBottleIsQuietOnARelocatableOne is the negative control: the
// same reference, reachable through a relative rpath, is not a finding.
func TestAuditMirroredBottleIsQuietOnARelocatableOne(t *testing.T) {
	tb := gzBottle(t, map[string][]byte{
		"lloyd.github.io/yajl/v2.1.0/lib/libyajl.2.dylib": machoBytes(
			[2]any{lcLoad, "@rpath/zlib.net/v1.3.2/lib/libz.1.dylib"},
			[2]any{lcRpath, "@loader_path/../../../.."},
			[2]any{lcRpath, "/Users/runner/.pkgx"}),
	})
	checked, problems, err := auditMirroredBottle(tb, bottle.ExtTarGz, "lloyd.github.io/yajl", "2.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if checked != 1 || len(problems) != 0 {
		t.Errorf("checked=%d problems=%v, want 1 and none", checked, problems)
	}
}

// TestAuditMirroredBottleSaysWhenItCouldNotRead: a compression the audit cannot
// open must report an error, never zero findings — a scan that could not read
// is not a scan that found nothing.
func TestAuditMirroredBottleSaysWhenItCouldNotRead(t *testing.T) {
	tb := gzBottle(t, map[string][]byte{"x/v1/bin/x": []byte("hi")})
	_, _, err := auditMirroredBottle(tb, ".tar.br", "x", "1")
	if err == nil {
		t.Fatal("an unhandled compression returned no error")
	}
	if !strings.Contains(err.Error(), "unhandled compression") {
		t.Errorf("error does not name the cause: %v", err)
	}
}
