package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-attest/sbom"
	"github.com/go-attest/sbom/provenance"
	"github.com/go-attest/sign"
	"github.com/go-pkgx/bottle"
	"github.com/ulikunitz/xz"
)

// miniOCI is a tiny in-memory OCI registry — just enough of the distribution
// API for one PushWithReferrers (ping, blob upload, manifest PUT with an
// OCI-Subject echo so ORAS skips fallback-tag maintenance).
type miniOCI struct {
	srv *httptest.Server
}

func newMiniOCI() *miniOCI {
	m := &miniOCI{}
	blobs := map[string]int{}        // digest -> size
	manifests := map[string][]byte{} // tag or digest -> manifest/index body
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/v2/" || p == "/v2":
			w.WriteHeader(200)
		case strings.Contains(p, "/blobs/uploads/"):
			if r.Method == "POST" {
				w.Header().Set("Location", strings.SplitN(p, "/blobs/", 2)[0]+"/blobs/uploads/u1")
				w.WriteHeader(202)
			} else { // PUT: monolithic upload
				body, _ := io.ReadAll(r.Body)
				blobs[r.URL.Query().Get("digest")] = len(body)
				w.Header().Set("Docker-Content-Digest", r.URL.Query().Get("digest"))
				w.WriteHeader(201)
			}
		case strings.Contains(p, "/blobs/"):
			dg := p[strings.LastIndex(p, "/")+1:]
			if sz, ok := blobs[dg]; ok {
				w.Header().Set("Content-Length", strconv.Itoa(sz))
				w.Header().Set("Docker-Content-Digest", dg)
				w.WriteHeader(200)
			} else {
				w.WriteHeader(404)
			}
		case strings.Contains(p, "/manifests/"):
			ref := p[strings.LastIndex(p, "/")+1:]
			switch r.Method {
			case "PUT":
				body, _ := io.ReadAll(r.Body)
				var man struct {
					Subject *struct {
						Digest string `json:"digest"`
					} `json:"subject"`
				}
				if json.Unmarshal(body, &man) == nil && man.Subject != nil {
					w.Header().Set("OCI-Subject", man.Subject.Digest)
				}
				s := sha256.Sum256(body)
				dg := "sha256:" + hex.EncodeToString(s[:])
				manifests[ref] = body // by tag (or digest, if pushed by digest)
				manifests[dg] = body  // always addressable by digest
				w.Header().Set("Docker-Content-Digest", dg)
				w.WriteHeader(201)
			default: // HEAD/GET — serve the stored manifest so read-modify-write
				// of the version-tag index (bottle's reconcile) can read it back.
				body, ok := manifests[ref]
				if !ok {
					w.WriteHeader(404)
					return
				}
				var probe struct {
					MediaType string `json:"mediaType"`
				}
				_ = json.Unmarshal(body, &probe)
				if probe.MediaType == "" {
					probe.MediaType = "application/vnd.oci.image.manifest.v1+json"
				}
				s := sha256.Sum256(body)
				w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(s[:]))
				w.Header().Set("Content-Type", probe.MediaType)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				if r.Method == "GET" {
					w.WriteHeader(200)
					_, _ = w.Write(body)
				} else {
					w.WriteHeader(200)
				}
			}
		default:
			w.WriteHeader(404)
		}
	}))
	return m
}

func (m *miniOCI) base() string {
	return "oci://" + strings.TrimPrefix(m.srv.URL, "http://") + "/go-pkgx/bottles"
}
func (m *miniOCI) close() { m.srv.Close() }

var errBoom = errors.New("boom")

func writeBottle(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("bottle-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPublishEndToEnd drives the real ociPush closure against the mini registry,
// proving the bk→bottle→referrer chain.
func TestPublishEndToEnd(t *testing.T) {
	m := newMiniOCI()
	defer m.close()
	b := writeBottle(t, "sqlite.tar.gz")
	code, out, errs := run2(t, "publish", "--to", m.base(), "--project", "sqlite.org",
		"--version", "3.46.0", "--platform", "darwin/x86-64", b)
	if code != 0 {
		t.Fatalf("publish code=%d err=%q", code, errs)
	}
	if !strings.Contains(out, "published sqlite.org 3.46.0 darwin/x86-64") || !strings.Contains(out, "+SBOM +provenance") {
		t.Errorf("output = %q", out)
	}
	// a .tar.xz bottle exercises the xz layer-media branch
	bx := writeBottle(t, "x.tar.xz")
	if c, _, e := run2(t, "publish", "--to", m.base(), "--project", "z.org",
		"--version", "1", "--platform", "linux/aarch64", bx); c != 0 {
		t.Fatalf("xz publish code=%d err=%q", c, e)
	}
}

func TestPublishSigned(t *testing.T) {
	m := newMiniOCI()
	defer m.close()
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(keyFile, []byte(kp.SecretKeyFile("")), 0o600); err != nil {
		t.Fatal(err)
	}
	b := writeBottle(t, "s.tar.gz")
	code, out, errs := run2(t, "publish", "--to", m.base(), "--project", "z.org",
		"--version", "1", "--platform", "linux/x86-64", "--sign", keyFile, b)
	if code != 0 {
		t.Fatalf("signed publish code=%d err=%q", code, errs)
	}
	if !strings.Contains(out, "+signature") {
		t.Errorf("output = %q", out)
	}
	// unreadable key
	if c, _, _ := run2(t, "publish", "--to", m.base(), "--project", "z.org", "--version", "1",
		"--platform", "linux/x86-64", "--sign", filepath.Join(t.TempDir(), "nope"), b); c != 1 {
		t.Error("missing key file")
	}
	// invalid key content
	bad := filepath.Join(t.TempDir(), "bad.key")
	os.WriteFile(bad, []byte("garbage"), 0o600)
	if c, _, _ := run2(t, "publish", "--to", m.base(), "--project", "z.org", "--version", "1",
		"--platform", "linux/x86-64", "--sign", bad, b); c != 1 {
		t.Error("bad key content")
	}
}

func TestPublishFlagErrors(t *testing.T) {
	if c, _, _ := run2(t, "publish"); c != 2 {
		t.Error("no flags")
	}
	if c, _, _ := run2(t, "publish", "-nope"); c != 2 {
		t.Error("bad flag")
	}
	if c, _, _ := run2(t, "publish", "--to", "oci://x/y", "--project", "p"); c != 2 {
		t.Error("missing required")
	}
	if c, _, _ := run2(t, "publish", "--to", "oci://x/y", "--project", "p", "--version", "1",
		"--platform", "bad", filepath.Join(t.TempDir(), "f")); c != 2 {
		t.Error("bad platform")
	}
}

func TestPublishRuntimeErrors(t *testing.T) {
	// unreadable bottle
	if c, _, _ := run2(t, "publish", "--to", "oci://x/y", "--project", "p", "--version", "1",
		"--platform", "linux/x86-64", filepath.Join(t.TempDir(), "missing")); c != 1 {
		t.Error("missing bottle")
	}
	b := writeBottle(t, "b.tar.gz")
	// buildReferrers failure via the sbom seam
	os1 := sbomJSON
	sbomJSON = func(sbom.Document) ([]byte, error) { return nil, errBoom }
	if c, _, _ := run2(t, "publish", "--to", "oci://x/y", "--project", "p", "--version", "1",
		"--platform", "linux/x86-64", b); c != 1 {
		t.Error("sbom error")
	}
	sbomJSON = os1
	// ociPush failure via the seam
	op := ociPush
	ociPush = func(string, string, string, string, string, []byte, string, []bottle.Referrer, map[string]string) error {
		return errBoom
	}
	if c, _, _ := run2(t, "publish", "--to", "oci://x/y", "--project", "p", "--version", "1",
		"--platform", "linux/x86-64", b); c != 1 {
		t.Error("push error")
	}
	ociPush = op
}

func TestBuildReferrers(t *testing.T) {
	tarball := []byte("x")
	refs, err := buildReferrers("sqlite.org", "3.46.0", "linux", "x86-64", tarball, time.Unix(0, 0).UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ArtifactType != artifactCycloneDX || refs[1].ArtifactType != artifactInToto {
		t.Fatalf("refs = %+v", refs)
	}
	// blobs are valid JSON of the expected kind
	if !json.Valid(refs[0].Blob) || !strings.Contains(string(refs[0].Blob), "CycloneDX") {
		t.Errorf("sbom blob: %s", refs[0].Blob)
	}
	if !json.Valid(refs[1].Blob) || !strings.Contains(string(refs[1].Blob), "in-toto") {
		t.Errorf("prov blob: %s", refs[1].Blob)
	}

	// with a signing key: a third, signature referrer that VerifySignature accepts
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	sref, err := buildReferrers("sqlite.org", "3.46.0", "linux", "x86-64", tarball, time.Unix(0, 0).UTC(), kp)
	if err != nil {
		t.Fatal(err)
	}
	if len(sref) != 3 || sref[2].ArtifactType != bottle.ArtifactTypeSignature {
		t.Fatalf("signed refs = %+v", sref)
	}
	sig := sref[2].Annotations[bottle.CosignSignatureAnnotation]
	if err := bottle.VerifySignature(tarball, sref[2].Blob, sig, kp.PublicKeyString()); err != nil {
		t.Errorf("signature referrer does not verify: %v", err)
	}

	// provenance seam error
	pj := provJSON
	provJSON = func(provenance.Statement) ([]byte, error) { return nil, errBoom }
	if _, err := buildReferrers("p", "1", "linux", "x86-64", tarball, time.Unix(0, 0).UTC(), nil); err == nil {
		t.Error("expected provenance error")
	}
	provJSON = pj

	// simple-signing seam error
	ss := simpleSigning
	simpleSigning = func(string, string) ([]byte, error) { return nil, errBoom }
	if _, err := buildReferrers("p", "1", "linux", "x86-64", tarball, time.Unix(0, 0).UTC(), kp); err == nil {
		t.Error("expected simple-signing error")
	}
	simpleSigning = ss
}

func TestBuildTime(t *testing.T) {
	// valid SOURCE_DATE_EPOCH → deterministic UTC
	t.Setenv("SOURCE_DATE_EPOCH", "1000000000")
	if got := buildTime(); !got.Equal(time.Unix(1000000000, 0).UTC()) {
		t.Errorf("epoch: %v", got)
	}
	on := nowFn
	nowFn = func() time.Time { return time.Unix(42, 0) }
	defer func() { nowFn = on }()
	// unparseable → wall clock
	t.Setenv("SOURCE_DATE_EPOCH", "nope")
	if got := buildTime(); !got.Equal(time.Unix(42, 0).UTC()) {
		t.Errorf("invalid fallback: %v", got)
	}
	// unset → wall clock
	os.Unsetenv("SOURCE_DATE_EPOCH")
	if got := buildTime(); !got.Equal(time.Unix(42, 0).UTC()) {
		t.Errorf("unset fallback: %v", got)
	}
}

func TestOCIPushBadBase(t *testing.T) {
	// the default ociPush must surface NewOCIClient's error for a non-oci base
	if err := ociPush("https://not-oci", "p", "1", "linux", "x86-64", []byte("x"), ".tar.gz", nil, nil); err == nil {
		t.Error("expected NewOCIClient error")
	}
}

func TestSplitPlatform(t *testing.T) {
	for _, c := range []struct {
		in, os, arch string
		ok           bool
	}{
		{"linux/x86-64", "linux", "x86-64", true},
		{"darwin/aarch64", "darwin", "aarch64", true},
		{"noslash", "", "", false},
		{"/leading", "", "", false},
		{"trailing/", "", "", false},
	} {
		os, arch, ok := splitPlatform(c.in)
		if os != c.os || arch != c.arch || ok != c.ok {
			t.Errorf("splitPlatform(%q) = %q,%q,%v", c.in, os, arch, ok)
		}
	}
}

// TestPublishGlibcTag: --glibc flavors ONLY the registry tag (not the SBOM
// version), so builds of one version against different glibc coexist.
func TestPublishGlibcTag(t *testing.T) {
	b := writeBottle(t, "g.tar.gz")
	op := ociPush
	defer func() { ociPush = op }()
	for _, tc := range []struct{ flag, wantTag, wantGlibcAnn string }{
		{"2.27.0", "3.46.0-glibc2.27.0", "2.27.0"},
		{"=2.27.0", "3.46.0-glibc2.27.0", "2.27.0"}, // a leading "=" is not doubled
		{"", "3.46.0", ""},                          // unset → plain tag, no annotation
	} {
		var gotTag string
		var gotAnn map[string]string
		ociPush = func(_, _, ver, _, _ string, _ []byte, _ string, _ []bottle.Referrer, ann map[string]string) error {
			gotTag = ver
			gotAnn = ann
			return nil
		}
		args := []string{"publish", "--to", "oci://r.example/x", "--project", "curl.se",
			"--version", "3.46.0", "--platform", "linux/x86-64"}
		if tc.flag != "" {
			args = append(args, "--glibc", tc.flag)
		}
		args = append(args, b)
		if code, _, errs := run2(t, args...); code != 0 {
			t.Fatalf("glibc=%q publish code=%d err=%q", tc.flag, code, errs)
		}
		if gotTag != tc.wantTag {
			t.Errorf("glibc=%q: pushed tag = %q, want %q", tc.flag, gotTag, tc.wantTag)
		}
		if gotAnn[bottle.GlibcVersionAnnotation] != tc.wantGlibcAnn {
			t.Errorf("glibc=%q: glibc.version annotation = %q, want %q", tc.flag, gotAnn[bottle.GlibcVersionAnnotation], tc.wantGlibcAnn)
		}
	}
}

// --- glibc min-kernel extraction ------------------------------------------

// abiNoteELF crafts a minimal ELF64 with a .note.ABI-tag encoding kernel maj.min.sub.
func abiNoteELF(maj, min, sub uint32) []byte {
	le := binary.LittleEndian
	// note: namesz=4("GNU\0") descsz=16 type=1 | "GNU\0" | [os=0,maj,min,sub]
	note := make([]byte, 12)
	le.PutUint32(note[0:], 4)
	le.PutUint32(note[4:], 16)
	le.PutUint32(note[8:], 1)
	note = append(note, 'G', 'N', 'U', 0)
	d := make([]byte, 16)
	le.PutUint32(d[4:], maj)
	le.PutUint32(d[8:], min)
	le.PutUint32(d[12:], sub)
	note = append(note, d...)

	var shstr bytes.Buffer
	shstr.WriteByte(0)
	nName := func(s string) uint32 { o := uint32(shstr.Len()); shstr.WriteString(s); shstr.WriteByte(0); return o }
	nNote := nName(".note.ABI-tag")
	nShstr := nName(".shstrtab")
	noteOff := uint64(64)
	shstrOff := noteOff + uint64(len(note))
	shoff := shstrOff + uint64(shstr.Len())

	b := &bytes.Buffer{}
	h := make([]byte, 64)
	copy(h[0:], []byte{0x7f, 'E', 'L', 'F'})
	h[4], h[5], h[6] = 2, 1, 1
	le.PutUint16(h[16:], 3)
	le.PutUint16(h[18:], 62)
	le.PutUint32(h[20:], 1)
	le.PutUint64(h[40:], shoff)
	le.PutUint16(h[52:], 64)
	le.PutUint16(h[58:], 64)
	le.PutUint16(h[60:], 3)
	le.PutUint16(h[62:], 2)
	b.Write(h)
	b.Write(note)
	b.Write(shstr.Bytes())
	sh := func(name, typ uint32, off, sz, align uint64) {
		s := make([]byte, 64)
		le.PutUint32(s[0:], name)
		le.PutUint32(s[4:], typ)
		le.PutUint64(s[24:], off)
		le.PutUint64(s[32:], sz)
		le.PutUint64(s[48:], align)
		b.Write(s)
	}
	sh(0, 0, 0, 0, 0)
	sh(nNote, 7, noteOff, uint64(len(note)), 4)
	sh(nShstr, 3, shstrOff, uint64(shstr.Len()), 1)
	return b.Bytes()
}

func tarBytes(files map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0o644})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

func gzBytes(b []byte) []byte {
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	_, _ = gw.Write(b)
	_ = gw.Close()
	return out.Bytes()
}

func xzBytes(t *testing.T, b []byte) []byte {
	var out bytes.Buffer
	xw, err := xz.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = xw.Write(b)
	_ = xw.Close()
	return out.Bytes()
}

func TestGlibcMinKernelFromTarball(t *testing.T) {
	libc := abiNoteELF(3, 2, 0)
	// gzip, real libc.so.6 nested under a version dir
	if mk, err := glibcMinKernelFromTarball(gzBytes(tarBytes(map[string][]byte{
		"gnu.org/glibc/v2.27.0/lib/glibc-2.27/libc.so.6": libc,
		"gnu.org/glibc/v2.27.0/lib/other.so":             []byte("x"),
	})), bottle.ExtTarGz); err != nil || mk != "3.2.0" {
		t.Fatalf("gz = %q, %v (want 3.2.0)", mk, err)
	}
	// xz path
	if mk, err := glibcMinKernelFromTarball(xzBytes(t, tarBytes(map[string][]byte{"a/libc.so.6": libc})), bottle.ExtTarXz); err != nil || mk != "3.2.0" {
		t.Fatalf("xz = %q, %v", mk, err)
	}
	// no libc.so.6
	if _, err := glibcMinKernelFromTarball(gzBytes(tarBytes(map[string][]byte{"a/other": libc})), bottle.ExtTarGz); err == nil {
		t.Error("missing libc.so.6 should error")
	}
	// libc.so.6 that is not an ELF
	if _, err := glibcMinKernelFromTarball(gzBytes(tarBytes(map[string][]byte{"a/libc.so.6": []byte("garbage")})), bottle.ExtTarGz); err == nil {
		t.Error("non-ELF libc.so.6 should error")
	}
	// bad gzip / bad xz
	if _, err := glibcMinKernelFromTarball([]byte("not gzip"), bottle.ExtTarGz); err == nil {
		t.Error("bad gzip should error")
	}
	if _, err := glibcMinKernelFromTarball([]byte("not xz"), bottle.ExtTarXz); err == nil {
		t.Error("bad xz should error")
	}
	// corrupt tar header -> tr.Next error (a full 512-byte block of garbage)
	if _, err := glibcMinKernelFromTarball(gzBytes(bytes.Repeat([]byte{0xff}, 512)), bottle.ExtTarGz); err == nil {
		t.Error("corrupt tar header should error")
	}
	// truncated entry -> io.Copy error: full header (size 2000) but the stream is
	// cut mid-data, so copying the libc.so.6 body hits an unexpected EOF.
	full := tarBytes(map[string][]byte{"a/libc.so.6": make([]byte, 2000)})
	if _, err := glibcMinKernelFromTarball(gzBytes(full[:612]), bottle.ExtTarGz); err == nil {
		t.Error("truncated entry should error on io.Copy")
	}
	// osCreateTemp failure via the seam
	oc := osCreateTemp
	osCreateTemp = func(string, string) (*os.File, error) { return nil, errBoom }
	if _, err := glibcMinKernelFromTarball(gzBytes(tarBytes(map[string][]byte{"a/libc.so.6": libc})), bottle.ExtTarGz); err == nil {
		t.Error("osCreateTemp failure should propagate")
	}
	osCreateTemp = oc
}

// TestGlibcMinKernelPre234Layout: before glibc 2.34 a bottle ships the ELF as
// libc-X.Y.so and makes libc.so.6 a SYMLINK to it — tar carries no content for
// a symlink, so the floor has to come from the link's target. Real case: the
// upstream glibc 2.17/2.24/2.27 bottles are published exactly like this.
func TestGlibcMinKernelPre234Layout(t *testing.T) {
	libc := abiNoteELF(2, 6, 32)
	dir := "gnu.org/glibc/v2.17.0/lib/glibc-2.17/"
	tb := tarWith(
		reg(dir+"libc-2.17.so", libc),
		reg(dir+"libc.so", []byte("/* GNU ld script */")), // a linker script, NOT an ELF
		symlinkEntry(dir+"libc.so.6", "libc-2.17.so"),
	)
	if mk, err := glibcMinKernelFromTarball(gzBytes(tb), bottle.ExtTarGz); err != nil || mk != "2.6.32" {
		t.Fatalf("symlinked libc = %q, %v (want 2.6.32)", mk, err)
	}
	// a dangling libc.so.6 symlink is still a clean "not found"
	if _, err := glibcMinKernelFromTarball(gzBytes(tarWith(symlinkEntry(dir+"libc.so.6", "nowhere.so"))), bottle.ExtTarGz); err == nil {
		t.Error("a dangling libc.so.6 symlink should error")
	}
	// a regular libc.so.6 still wins over any versioned image present
	both := tarWith(reg(dir+"libc-2.17.so", abiNoteELF(9, 9, 9)), reg(dir+"libc.so.6", libc))
	if mk, err := glibcMinKernelFromTarball(gzBytes(both), bottle.ExtTarGz); err != nil || mk != "2.6.32" {
		t.Fatalf("regular libc.so.6 = %q, %v", mk, err)
	}
	// staging the ELF can fail on write (here: an already-closed temp file)
	oc := osCreateTemp
	osCreateTemp = func(dir, pattern string) (*os.File, error) {
		f, err := oc(dir, pattern)
		if err == nil {
			f.Close()
		}
		return f, err
	}
	defer func() { osCreateTemp = oc }()
	if _, err := glibcMinKernelFromTarball(gzBytes(both), bottle.ExtTarGz); err == nil {
		t.Error("a failing write to the staged ELF should propagate")
	}
}

// tarEntry is one member of a test tarball (ordered, unlike tarBytes' map).
type tarEntry struct {
	hdr  tar.Header
	data []byte
}

func reg(name string, data []byte) tarEntry {
	return tarEntry{tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0o644}, data}
}

func symlinkEntry(name, target string) tarEntry {
	return tarEntry{tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}, nil}
}

func tarWith(entries ...tarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		_ = tw.WriteHeader(&e.hdr)
		_, _ = tw.Write(e.data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// TestPublishGlibcBottleMinKernel: publishing gnu.org/glibc stamps min-kernel
// and does NOT flavor the tag.
func TestPublishGlibcBottleMinKernel(t *testing.T) {
	dir := t.TempDir()
	bp := filepath.Join(dir, "v2.27.0.tar.gz")
	if err := os.WriteFile(bp, gzBytes(tarBytes(map[string][]byte{
		"gnu.org/glibc/v2.27.0/lib/glibc-2.27/libc.so.6": abiNoteELF(3, 10, 0),
	})), 0o644); err != nil {
		t.Fatal(err)
	}
	op := ociPush
	defer func() { ociPush = op }()
	var gotTag string
	var gotAnn map[string]string
	ociPush = func(_, _, ver, _, _ string, _ []byte, _ string, _ []bottle.Referrer, ann map[string]string) error {
		gotTag, gotAnn = ver, ann
		return nil
	}
	if code, _, errs := run2(t, "publish", "--to", "oci://r.example/x", "--project", bottle.GlibcProject,
		"--version", "2.27.0", "--platform", "linux/x86-64", bp); code != 0 {
		t.Fatalf("glibc publish code=%d err=%q", code, errs)
	}
	if gotTag != "2.27.0" {
		t.Errorf("glibc tag = %q, want unflavored 2.27.0", gotTag)
	}
	if gotAnn[bottle.GlibcMinKernelAnnotation] != "3.10.0" {
		t.Errorf("min-kernel annotation = %q, want 3.10.0", gotAnn[bottle.GlibcMinKernelAnnotation])
	}
	// a glibc tarball WITHOUT libc.so.6 fails the publish
	bad := filepath.Join(dir, "v9.tar.gz")
	os.WriteFile(bad, gzBytes(tarBytes(map[string][]byte{"x/y": []byte("z")})), 0o644)
	if code, _, _ := run2(t, "publish", "--to", "oci://r.example/x", "--project", bottle.GlibcProject,
		"--version", "9", "--platform", "linux/x86-64", bad); code != 1 {
		t.Error("glibc bottle without libc.so.6 should fail")
	}
}
