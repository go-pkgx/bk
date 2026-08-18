package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-attest/sbom"
	"github.com/go-attest/sbom/provenance"
	"github.com/go-attest/sign"
	"github.com/go-pkgx/bottle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/ulikunitz/xz"
)

// OCI artifactType / mediaType of each attestation attached as a referrer.
const (
	artifactCycloneDX = "application/vnd.cyclonedx+json"
	artifactInToto    = "application/vnd.in-toto+json"
	buildType         = "https://github.com/go-pkgx/bk/buildtype@v1"
	builderID         = "https://github.com/go-pkgx/bk"
)

// seams (swapped in tests).
var (
	osReadFile    = os.ReadFile
	osCreateTemp  = os.CreateTemp
	nowFn         = time.Now
	sbomJSON      = func(d sbom.Document) ([]byte, error) { return d.CycloneDX() }
	provJSON      = func(s provenance.Statement) ([]byte, error) { return s.JSON() }
	simpleSigning = sign.SimpleSigningPayload
	ociPush       = func(distBase, project, ver, osn, arch string, tarball []byte, ext string, refs []bottle.Referrer, annotations map[string]string) (ocispec.Descriptor, error) {
		c, err := bottle.NewOCIClient(distBase)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		// The DESCRIPTOR, not just an error: the end-of-run index check needs to
		// name the manifest it is looking for, and it cannot discover it — that
		// lookup would go through the index whose contents are in question.
		return c.PushWithReferrersAnnotated(project, ver, osn, arch, tarball, ext, refs, annotations)
	}
)

// libcSoname is the C library's soname — the ELF whose .note.ABI-tag records
// glibc's minimum supported kernel.
const libcSoname = "libc.so.6"

// glibcMinKernelFromTarball finds the C library inside a bottle tarball and
// returns glibc's minimum supported kernel (from its .note.ABI-tag) — the value
// stamped as the org.go-pkgx.glibc.min-kernel annotation on a glibc bottle so a
// glibc-by-kernel selector can pick it. ext is ".tar.gz" or ".tar.xz".
//
// Where the ELF lives depends on the glibc vintage: a modern bottle ships
// lib/glibc-X.Y/libc.so.6 as a regular file, while glibc before 2.34 ships the
// image as libc-X.Y.so with libc.so.6 a SYMLINK to it (tar carries no content
// for a symlink). Both are handled — and `libc.so` is deliberately not, since
// that one is a linker script, not an ELF.
func glibcMinKernelFromTarball(tarball []byte, ext string) (string, error) {
	var r io.Reader = bytes.NewReader(tarball)
	if ext == bottle.ExtTarXz {
		xr, err := xz.NewReader(r)
		if err != nil {
			return "", err
		}
		r = xr
	} else {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return "", err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	var linkTarget string         // what libc.so.6 points at, when it is a symlink
	images := map[string][]byte{} // candidate ELFs, by base name
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		base := filepath.Base(h.Name)
		switch {
		case h.Typeflag == tar.TypeSymlink && base == libcSoname:
			linkTarget = filepath.Base(h.Linkname)
		case h.Typeflag == tar.TypeReg && (base == libcSoname || isLibcImage(base)):
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			images[base] = b
		}
	}
	image, ok := images[libcSoname]
	if !ok {
		image, ok = images[linkTarget]
	}
	if !ok {
		return "", fmt.Errorf("%s not found in tarball", libcSoname)
	}
	// GlibcMinKernel wants a path; stage the ELF to a temp file.
	f, err := osCreateTemp("", "libc-*.so.6")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(image); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	return bottle.GlibcMinKernel(f.Name())
}

// isLibcImage reports whether a file name is glibc's versioned C library image
// (libc-2.17.so), the real ELF that a pre-2.34 bottle's libc.so.6 links to.
func isLibcImage(base string) bool {
	return strings.HasPrefix(base, "libc-") && strings.HasSuffix(base, ".so")
}

// runPublish implements `bk publish`: push a built bottle to an OCI registry
// with a CycloneDX SBOM and an in-toto SLSA provenance statement attached as
// referrers.
func runPublish(args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("publish", flag.ContinueOnError)
	f.SetOutput(stderr)
	to := f.String("to", "", "oci:// destination base, e.g. oci://ghcr.io/go-pkgx/bottles")
	project := f.String("project", "", "pkgx project, e.g. sqlite.org")
	version := f.String("version", "", "version, e.g. 3.46.0")
	platform := f.String("platform", "", "bottle os/arch, e.g. darwin/x86-64")
	signKey := f.String("sign", "", "sign the bottle with this go-attest/sign secret key file")
	glibc := f.String("glibc", "", "glibc version this bottle was built against; publishes under the flavored tag <version>-glibc<ver> so builds against different glibc coexist (linux only)")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if *to == "" || *project == "" || *version == "" || *platform == "" || f.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bk publish --to oci://… --project P --version V --platform os/arch <bottle>")
		return 2
	}
	osn, arch, ok := splitPlatform(*platform)
	if !ok {
		fmt.Fprintf(stderr, "publish: bad --platform %q (want os/arch)\n", *platform)
		return 2
	}
	var kp *sign.Keypair
	if *signKey != "" {
		k, err := loadSigningKey(*signKey)
		if err != nil {
			fmt.Fprintln(stderr, "publish:", err)
			return 1
		}
		kp = k
	}
	if _, _, err := publishBottle(publishOptions{
		Dist: *to, Project: *project, Version: *version,
		OS: osn, Arch: arch, Path: f.Arg(0), Glibc: *glibc, Key: kp,
	}); err != nil {
		fmt.Fprintln(stderr, "publish:", err)
		return 1
	}
	extra := ""
	if kp != nil {
		extra = " +signature"
	}
	fmt.Fprintf(stdout, "published %s %s %s/%s to %s (+SBOM +provenance%s)\n", *project, *version, osn, arch, *to, extra)
	return 0
}

// loadSigningKey reads a go-attest/sign secret key file.
func loadSigningKey(path string) (*sign.Keypair, error) {
	kb, err := osReadFile(path)
	if err != nil {
		return nil, err
	}
	return sign.LoadSecretKey(string(kb))
}

// publishOptions describes one bottle push.
type publishOptions struct {
	Dist     string // oci:// destination base
	Project  string // pkgx project slug
	Version  string // TRUE software version (what the SBOM/provenance record)
	OS, Arch string // pkgx-form platform, e.g. linux + x86-64
	Path     string // the built bottle tarball on disk
	Glibc    string // glibc this bottle was built against ("" = system glibc)
	Key      *sign.Keypair
	Time     time.Time // attestation timestamp; zero = buildTime()
}

// publishBottle pushes a built bottle with its SBOM, SLSA provenance and (given
// a key) signature attached as referrers, and reports the registry tag it landed
// under. Shared by `bk publish` and `bk factory`, so a factory-published bottle
// carries byte-identical attestations, tags and glibc annotations.
// publishBottle returns the registry tag it published under AND the descriptor
// of the per-platform manifest it pushed. The descriptor is what the end-of-run
// index check needs: it cannot discover the manifest, because that lookup would
// go through the very index whose contents are in question.
func publishBottle(o publishOptions) (string, ocispec.Descriptor, error) {
	tarball, err := osReadFile(o.Path)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	// The FILE decides the media type, not a flag: a bottle mirrored from an
	// upstream dist arrives in whatever codec that dist used, and mislabelling it
	// would make every puller decompress with the wrong decoder.
	ext := bottle.ExtTarGz
	switch {
	case strings.HasSuffix(o.Path, bottle.ExtTarXz):
		ext = bottle.ExtTarXz
	case strings.HasSuffix(o.Path, bottle.ExtTarZst):
		ext = bottle.ExtTarZst
	}
	// The SBOM/provenance keep the TRUE software version; only the registry TAG
	// gets the glibc flavor, so builds of the same version against different
	// glibc coexist as distinct artifacts (e.g. curl.se:8.20-glibc2.27.0 vs
	// :8.20-glibc2.44.0) instead of colliding on one tag.
	when := o.Time
	if when.IsZero() {
		when = buildTime()
	}
	refs, err := buildReferrers(o.Project, o.Version, o.OS, o.Arch, tarball, when, o.Key)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	tag := flavoredTag(o.Project, o.Version, o.Glibc)
	var annotations map[string]string
	switch {
	case o.Project == bottle.GlibcProject:
		// The glibc bottle IS the glibc — no flavored tag; instead self-describe
		// its min supported kernel (from libc.so.6's .note.ABI-tag) so a
		// glibc-by-kernel selector can pick it.
		mk, err := glibcMinKernelFromTarball(tarball, ext)
		if err != nil {
			return "", ocispec.Descriptor{}, err
		}
		annotations = map[string]string{bottle.GlibcMinKernelAnnotation: mk}
	case o.Glibc != "":
		// A tool built against a specific glibc: flavor the tag + self-describe
		// which glibc, so a glibc-aware resolver matches it without parsing tags.
		annotations = map[string]string{bottle.GlibcVersionAnnotation: strings.TrimPrefix(o.Glibc, "=")}
	}
	desc, err := ociPush(o.Dist, o.Project, tag, o.OS, o.Arch, tarball, ext, refs, annotations)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	return tag, desc, nil
}

// flavoredTag is the registry tag a (project, version) lands under: the plain
// version, or <version>-glibc<ver> for a tool built against a pinned glibc. The
// glibc package itself is never flavored (it IS the glibc). The factory computes
// the same tag to test whether a bottle is already published.
func flavoredTag(project, version, glibc string) string {
	if glibc == "" || project == bottle.GlibcProject {
		return version
	}
	return version + "-glibc" + strings.TrimPrefix(glibc, "=")
}

// buildReferrers builds the CycloneDX SBOM and in-toto SLSA provenance
// attestations for a bottle (subject = the bottle itself; the tarball digest
// binds them).
func buildReferrers(project, version, osn, arch string, tarball []byte, now time.Time, kp *sign.Keypair) ([]bottle.Referrer, error) {
	sum := sha256.Sum256(tarball)
	dg := hex.EncodeToString(sum[:])
	purl := fmt.Sprintf("pkg:pkgx/%s@%s", project, version)

	doc := sbom.Document{Name: project, Version: version, PURL: purl, SHA256: dg, Created: now}
	sb, err := sbomJSON(doc)
	if err != nil {
		return nil, err
	}
	stmt := provenance.Statement{
		Subjects:   []provenance.Subject{{Name: fmt.Sprintf("%s %s %s/%s", project, version, osn, arch), SHA256: dg}},
		BuildType:  buildType,
		BuilderID:  builderID,
		StartedOn:  now,
		FinishedOn: now,
	}
	pr, err := provJSON(stmt)
	if err != nil {
		return nil, err
	}
	refs := []bottle.Referrer{
		{ArtifactType: artifactCycloneDX, MediaType: artifactCycloneDX, Blob: sb},
		{ArtifactType: artifactInToto, MediaType: artifactInToto, Blob: pr},
	}
	if kp != nil {
		payload, err := simpleSigning(purl, "sha256:"+dg)
		if err != nil {
			return nil, err
		}
		refs = append(refs, bottle.Referrer{
			ArtifactType: bottle.ArtifactTypeSignature,
			MediaType:    bottle.MediaSimpleSigning,
			Blob:         payload,
			Annotations:  map[string]string{bottle.CosignSignatureAnnotation: kp.SignPayload(payload)},
		})
	}
	return refs, nil
}

// buildTime is the timestamp stamped into attestations. It honours
// SOURCE_DATE_EPOCH (the reproducible-builds standard) so re-publishing the
// same bottle yields byte-identical SBOM/provenance — hence the same referrer
// digests, so a re-push is idempotent instead of accumulating duplicates. With
// the variable unset (or unparseable) it falls back to the wall clock.
func buildTime() time.Time {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0).UTC()
		}
	}
	return nowFn().UTC()
}

// splitPlatform parses "os/arch".
func splitPlatform(p string) (osn, arch string, ok bool) {
	i := strings.IndexByte(p, '/')
	if i <= 0 || i == len(p)-1 {
		return "", "", false
	}
	return p[:i], p[i+1:], true
}
