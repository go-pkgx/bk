package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-attest/sbom"
	"github.com/go-attest/sbom/provenance"
	"github.com/go-attest/sign"
	"github.com/go-pkgx/bottle"
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
	nowFn         = time.Now
	sbomJSON      = func(d sbom.Document) ([]byte, error) { return d.CycloneDX() }
	provJSON      = func(s provenance.Statement) ([]byte, error) { return s.JSON() }
	simpleSigning = sign.SimpleSigningPayload
	ociPush       = func(distBase, project, ver, osn, arch string, tarball []byte, ext string, refs []bottle.Referrer) error {
		c, err := bottle.NewOCIClient(distBase)
		if err != nil {
			return err
		}
		_, err = c.PushWithReferrers(project, ver, osn, arch, tarball, ext, refs)
		return err
	}
)

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
	path := f.Arg(0)
	tarball, err := osReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "publish:", err)
		return 1
	}
	ext := ".tar.gz"
	if strings.HasSuffix(path, ".tar.xz") {
		ext = ".tar.xz"
	}
	var kp *sign.Keypair
	if *signKey != "" {
		kb, err := osReadFile(*signKey)
		if err != nil {
			fmt.Fprintln(stderr, "publish:", err)
			return 1
		}
		if kp, err = sign.LoadSecretKey(string(kb)); err != nil {
			fmt.Fprintln(stderr, "publish:", err)
			return 1
		}
	}
	refs, err := buildReferrers(*project, *version, osn, arch, tarball, buildTime(), kp)
	if err != nil {
		fmt.Fprintln(stderr, "publish:", err)
		return 1
	}
	if err := ociPush(*to, *project, *version, osn, arch, tarball, ext, refs); err != nil {
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
