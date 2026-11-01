package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-attest/sbom"
	"github.com/go-attest/sbom/provenance"

	"github.com/go-attest/sign"
	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/moustache"
	"github.com/go-pkgx/bottle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// OCI artifactType / mediaType of each attestation attached as a referrer.
const (
	artifactCycloneDX = "application/vnd.cyclonedx+json"
	artifactInToto    = "application/vnd.in-toto+json"
	// artifactOpenVEX is the media type an OpenVEX document is attached under.
	// It is what makes the rebuild's JUSTIFICATION travel with the bottle
	// rather than live in a repository nobody downstream reads.
	artifactOpenVEX = "application/vnd.openvex+json"
	buildType       = "https://github.com/go-pkgx/bk/buildtype@v1"
	builderID       = "https://github.com/go-pkgx/bk"
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
// ABIProvidesAnnotation lists the sonames a bottle's shared libraries call
// themselves by, comma-separated and sorted.
//
// The compatibility key of a shared library is its soname, and a pkgx
// constraint cannot express one: libxml2 2.13.9 ships libxml2.2.dylib and
// 2.15.4 ships libxml2.16.dylib, and `^2` admits both. Recording what a bottle
// PROVIDES is the half that can be computed locally; with it, a checker can ask
// of any published version whether it still provides the soname a dependent
// recorded, and refuse a constraint that cannot honour it
// (go-pkgx/packages#207).
//
// It belongs beside GlibcVersionAnnotation in go-pkgx/bottle once a resolver
// reads it; today only bk writes it, so it lives here rather than forcing a
// cross-repo bump for a field nothing consumes yet.
const ABIProvidesAnnotation = "org.go-pkgx.abi.provides"

// sonamesFromTarball reads the ABI name of every shared library in a bottle.
//
// bottle.Decompressor, not a local one: every format this factory has ever
// published is handled there, and a reader taught two of the three fails with
// "gzip: invalid header" on a .tar.zst — which is the default, so the first
// version of this wrote the annotation on nothing at all.
//
// From the tarball rather than the build tree so it also describes a bottle we
// did not build — a mirrored one carries whatever upstream linked, and that is
// exactly the case where nobody knows what is inside (go-pkgx/packages#147).
//
// ⛔ ONLY FILES A CONSUMER COULD EVER LOAD. It used to read EVERY regular file
// in the tarball, and the published go.dev 1.27.3 bottle therefore announced
//
//	org.go-pkgx.abi.provides = libtiffxx.so.6
//
// on darwin/aarch64 AND linux/x86-64 — because the Go distribution ships
// `src/debug/elf/testdata/libtiffxx.so_`, a TEST FIXTURE of the standard
// library's ELF reader. Go ships no shared libraries at all. Reported as the
// second observation on go-pkgx/pkgm#73, where the stray `abi-libtiffxx.so.6
// -> v1.27.3` symlink turned up inside the go.dev store directory.
//
// That is not cosmetic: the whole point of the annotation is that the resolver
// keys ABI compatibility on sonames (go-pkgx/bottle#83) and a consumer binds
// through `abi-<soname>`. A claim to provide libtiffxx.so.6 is an offer to
// satisfy a libtiff consumer with the Go toolchain.
//
// So the publisher now describes exactly what the consumer can search:
// bottle.LibDirs puts `<prefix>/lib`, `<prefix>/lib64` and `lib/glibc-*` on
// LD_LIBRARY_PATH and nothing else, so a soname found anywhere else is a claim
// nothing could honour. Judge and subject count alike.
func sonamesFromTarball(tarball []byte, ext string) ([]string, error) {
	r, closeFn, err := bottle.Decompressor(ext, bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	defer closeFn()
	var out []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// Regular files only: a symlink IS the soname in most layouts
		// (libz.1.dylib -> libz.1.3.2.dylib) and would double-count.
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// And only where a consumer would look for it. Reading the rest is
		// what published libtiffxx.so.6 as an ABI of the Go toolchain.
		if !inLibDir(h.Name) {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if s := fixup.SonameOf(b); s != "" {
			out = append(out, s)
		}
	}
	return fixup.SortedUnique(out), nil
}

// inLibDir reports whether a tarball path is inside a directory the consumer
// puts on LD_LIBRARY_PATH. The list is bottle.LibDirs' own: `lib`, `lib64`,
// and lib's `glibc-*` sub-libdir.
//
// By path COMPONENT, not by prefix: `libexec/` starts with "lib" and holds
// helper programs nobody links against, and `src/.../testdata/` is where the
// defect came from. The components are checked all the way down rather than
// only at the top, because glibc's real libraries live in `lib/glibc-2.41/`
// and a bottle built with a `--libdir` of its own can nest one deeper.
func inLibDir(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	// The last element is the file itself, so a bare `lib` file is not "in"
	// a lib dir — there has to be a directory component above it.
	for _, p := range parts[:max(len(parts)-1, 0)] {
		if p == "lib" || p == "lib64" {
			return true
		}
	}
	return false
}

// bottle.Decompressor, for the reason sonamesFromTarball gives directly above
// — and this function is where that reason was NOT applied. It knew xz and
// assumed gzip for everything else, so on a .tar.zst, which is the default
// codec, glibc's publish died on
//
//	❌ PUBLISH FAIL gnu.org/glibc 2.44: gzip: invalid header
//
// after a build that had taken hours. Nothing else showed it because nothing
// else calls this: it is glibc's alone.
//
// The comment above was written when the SAME defect was found in the function
// above. A warning left beside the code it warns about is not a fix for the
// code next to it.
func glibcMinKernelFromTarball(tarball []byte, ext string) (string, error) {
	r, closeFn, err := bottle.Decompressor(ext, bytes.NewReader(tarball))
	if err != nil {
		return "", err
	}
	defer closeFn()
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
	advisories := f.String("advisories", "", "advisory tree to look this project up in; attaches an OpenVEX document saying what was decided about each vulnerability")
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
		Advisories: *advisories,
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
	// Source is what the build was made from — the candidate URL that answered
	// and the digest of what came back. Empty for `bk publish` on a bottle this
	// process did not build, and for the few recipes with no distributable.
	Source build.SourceRef
	// BuildEnv is every dependency the build resolved, at the version it used:
	// the compiler, its libc, the tools the recipe invoked. Empty for
	// `bk publish` on a bottle this process did not build — the same honest
	// gap as Source, and for the same reason.
	BuildEnv []moustache.Dep
	// Advisories is the advisory tree to look this project up in. Empty means
	// no VEX document is attached, which is the honest state for a project
	// nobody has triaged.
	Advisories string
	// Origin is what the factory knows about HOW this was built — see
	// provOrigin. Zero means nothing is stated, which is the honest value for
	// `bk publish` on a bottle this process did not build, and which a
	// promotion gate must treat as "not promotable" rather than as "fine".
	Origin provOrigin
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
	refs, err := buildReferrers(o.Project, o.Version, o.OS, o.Arch, tarball, o.Source, o.BuildEnv, when, o.Key, o.Origin)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	// The JUSTIFICATION, where there is one. A broken advisory file stops the
	// publish rather than becoming a missing document, because an absent VEX
	// reads as "nothing was ever found".
	vexRef, err := vexReferrer(o.Advisories, o.Project, o.Version, o.OS, o.Arch, tarball, when)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	if vexRef != nil {
		refs = append(refs, *vexRef)
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
	// Every bottle says which ABIs it provides, whatever else is annotated.
	// A failure to read one is not a reason to refuse the publish: the bottle is
	// correct, the description of it is merely missing, and the alternative is a
	// factory that stops on a field nothing consumes yet.
	if names, err := sonamesFromTarball(tarball, ext); err != nil {
		fmt.Fprintf(os.Stderr, "bk: could not read the sonames of %s %s: %v\n", o.Project, o.Version, err)
	} else if len(names) > 0 {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[ABIProvidesAnnotation] = strings.Join(names, ",")
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

// environmentMaterials renders the build environment as SLSA
// resolvedDependencies: every dependency the build resolved, at the version it
// actually used.
//
// ⛔ THIS IS THE AXIS A VULNERABILITY REBUILD MOVES ALONG. When a Go patch
// release fixes a standard-library defect, nothing about the recipe or the
// upstream version changes — only `go.dev` in this list does. A bottle that
// does not record it cannot be asked "were you built with the fixed
// compiler?", and a rebuild counter over such bottles would be a number
// nobody could derive or check.
//
// A URI of `pkg:pkgx/<project>@<version>` rather than a path: the prefix is
// where this machine put it, which is not a fact about the artefact. No digest
// is claimed, because we have the version the build resolved and not the bytes
// it read — and a material asserting a digest it did not verify would be the
// same empty promise this whole change exists to remove.
func environmentMaterials(env []moustache.Dep) []provenance.Material {
	out := make([]provenance.Material, 0, len(env))
	for _, d := range env {
		out = append(out, provenance.Material{URI: "pkg:pkgx/" + d.Project + "@" + d.Version})
	}
	return out
}

// builderWithVersion is the SLSA builder id, carrying the bk that ran.
//
// A build with no version information is not one you can ask anything about
// later. bkVersion() reads debug.ReadBuildInfo, so it cannot disagree with the
// binary; a `go run` of a working tree answers "(devel)", which is a true
// statement about a build nobody should publish from and is better recorded
// than hidden.
func builderWithVersion() string { return builderID + "@" + bkVersion() }

// buildParams is what the SLSA statement records as externalParameters: the
// question this build was asked.
//
// ⛔ IT WAS `{}`. The shape of a provenance was published, signed, for every
// bottle in the registry, and it said nothing — not the platform, not the
// version, not even the project. An attestation whose presence implies a
// guarantee it does not give is worse than no attestation, because a reader
// stops looking once they see one.
//
// Only what is KNOWN here goes in. The build-dependency closure and the spec
// hash are the two that matter most and are not in hand at this call; they
// arrive with the plumbing, not with a placeholder, because a field that is
// always empty teaches readers to ignore the block.
func buildParams(project, version, osn, arch string, o provOrigin) map[string]string {
	p := map[string]string{
		"project":  project,
		"version":  version,
		"platform": osn + "/" + arch,
	}
	// Emitted only when STATED,, for the reason the note above gives: a key
	// that is always there and always empty teaches a reader to skip it.
	if o.Bootstrap != "" {
		p["bootstrap"] = o.Bootstrap
	}
	if o.Dist != "" {
		p["inputs-from"] = o.Dist
	}
	return p
}

// provOrigin is what the FACTORY knows about a build and the bottle cannot
// show: whether host tools drove it, and which registry its inputs came from.
//
// ⛔ IT EXISTS BECAUSE "WAS THIS BUILT SOVEREIGNLY" HAD NO ANSWER IN THE
// ARTEFACT. Measured 2026-11-01 on a published bottle:
//
//	$ bk buildenv -platform linux/s390x lz4.org 1.10.0
//	buildenv: lz4.org 1.10.0 (linux/s390x): the provenance records no build
//	environment
//
// An empty build environment is what a `--bootstrap` build produces, because
// it resolves no bottles — so the absence is a HINT and not a statement. It is
// also what an older bk produced, and what `bk publish` produces for a bottle
// this process did not build. Three different situations, one silence.
//
// That silence matters now: go-pkgx/packages#316 needs the s390x toolchain to
// reach ghcr, generations 0 and 1 both publish only to a throwaway loopback
// registry on the VM, and the gate on promoting anything out of it is exactly
// "prove this was not built with the host's compiler". A gate cannot read a
// hint.
//
// ⛔ THREE STATES, NOT A BOOL, AND THE EMPTY ONE MUST NOT BE SAFE. A `bool`
// zero value is false, which would read as "not a bootstrap" — so every
// bottle published before this existed, and every one published by a path
// that does not set it, would claim to be promotable. Unknown has to fail
// closed, so it is a distinct state and the promotion check requires an
// explicit "0".
type provOrigin struct {
	// Bootstrap is "1" when the build took its compiler and shell from the
	// host distribution, "0" when it did not, and "" when nothing was stated.
	Bootstrap string
	// Dist is the registry the build RESOLVED ITS INPUTS FROM — the fact that
	// distinguishes a bottle built against the seed's loopback registry from
	// one built against ghcr, which no digest or signature can show.
	Dist string
}

// boolParam renders a known boolean the way buildParams records it.
func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// buildReferrers builds the CycloneDX SBOM and in-toto SLSA provenance
// attestations for a bottle (subject = the bottle itself; the tarball digest
// binds them).
func buildReferrers(project, version, osn, arch string, tarball []byte, src build.SourceRef, env []moustache.Dep, now time.Time, kp *sign.Keypair, origin provOrigin) ([]bottle.Referrer, error) {
	sum := sha256.Sum256(tarball)
	dg := hex.EncodeToString(sum[:])
	purl := fmt.Sprintf("pkg:pkgx/%s@%s", project, version)

	doc := sbom.Document{Name: project, Version: version, PURL: purl, SHA256: dg, Created: now}
	sb, err := sbomJSON(doc)
	if err != nil {
		return nil, err
	}
	stmt := provenance.Statement{
		Subjects:  []provenance.Subject{{Name: fmt.Sprintf("%s %s %s/%s", project, version, osn, arch), SHA256: dg}},
		BuildType: buildType,
		// ⛔ A BUILDER THAT NEVER CHANGES CANNOT TELL TWO BUILDS APART.
		// builderID was the bare string `https://github.com/go-pkgx/bk`, so
		// every bottle ever published claimed the identical builder —
		// measured 2026-10-09 on stedolan.github.io/jq, whose signed SLSA
		// statement carries buildDefinition {} and that constant.
		//
		// SLSA says builder.id must identify the build platform closely
		// enough that a change in it is a change in the id. A rebuild for a
		// toolchain security fix is exactly such a change, and the question
		// "which bk built this?" had no answer in the artefact.
		BuilderID:  builderWithVersion(),
		StartedOn:  now,
		FinishedOn: now,
		Params:     buildParams(project, version, osn, arch, origin),
		Materials:  append(materialsOf(src), environmentMaterials(env)...),
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

// materialsOf renders the build's source as SLSA resolvedDependencies.
//
// A material with no digest of any kind would say only "this URL was involved",
// which the recipe already says and which no verifier can check — so an empty
// SourceRef produces no material at all rather than a decorative one. That is
// also what `bk publish` emits for a bottle this process did not build: it does
// not know what the source was, and inventing a URI from the recipe would
// attest a guess.
func materialsOf(src build.SourceRef) []provenance.Material {
	if src.SHA256 == "" && src.Commit == "" {
		return nil
	}
	return []provenance.Material{{URI: src.URI, SHA256: src.SHA256, GitCommit: src.Commit}}
}
