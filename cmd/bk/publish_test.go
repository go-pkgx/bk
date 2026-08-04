package main

import (
	"crypto/sha256"
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
	ociPush = func(string, string, string, string, string, []byte, string, []bottle.Referrer) error { return errBoom }
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
	if err := ociPush("https://not-oci", "p", "1", "linux", "x86-64", []byte("x"), ".tar.gz", nil); err == nil {
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
