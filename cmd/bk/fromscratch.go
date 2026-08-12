package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bottle"
	"github.com/ulikunitz/xz"
)

// runFromscratch implements `bk fromscratch`: resolve a pantry project's full
// runtime closure to published ghcr bottles and build a genuine
// `docker FROM scratch` image for it — pure Go, no Python, no external
// readelf/patchelf. It absorbs the former closure.py + mkclosure.sh + mkscratch.sh:
//
//	closure-resolve (local pantry, host-regex + platform maps, glibc first)
//	  → pull each bottle from ghcr (anonymous OCI) → layout /pkgx/<proj>/v<ver>
//	  → symlink the glibc loader to the standard PT_INTERP path
//	  → auto-discover every shared-object dir → LD_LIBRARY_PATH
//	  → readelf-driven completion (debug/elf): pull providers of undeclared
//	    NEEDED sonames (libcrypt.so.1 → github.com/besser82/libxcrypt, …)
//	  → write a FROM-scratch Dockerfile → docker build (+ optional smoke -run).
func runFromscratch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fromscratch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	archFlag := fs.String("arch", "", "target arch: amd64|arm64 (aliases x86-64|aarch64)")
	tag := fs.String("tag", "", "docker image tag to build")
	entrypoint := fs.String("entrypoint", "", "in-image ENTRYPOINT; {V} = root version dir")
	pantryDir := fs.String("pantry", "pantry", "pantry checkout directory")
	resolveOnly := fs.Bool("resolve-only", false, "print the resolved proj:ver closure and exit (no image)")
	run := fs.Bool("run", false, "after building, docker-run the image with the args after `--` (smoke test)")
	keep := fs.Bool("keep", false, "keep the build workdir (debug)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Positional form: <root-project> [-- <smoke args…>]. Go's flag package
	// stops at the first non-flag, so the `--` separator (if any) lands in the
	// positional args after the project rather than being consumed — strip it.
	rest := fs.Args()
	if *archFlag == "" || len(rest) < 1 {
		fmt.Fprintln(stderr, "usage: bk fromscratch -arch <a> [-tag t -entrypoint e] [-pantry d] [-resolve-only] [-run -- args] <root-project>")
		return 2
	}
	rootProj := rest[0]
	smokeArgs := rest[1:]
	if len(smokeArgs) > 0 && smokeArgs[0] == "--" {
		smokeArgs = smokeArgs[1:]
	}

	a, err := resolveFsArch(*archFlag)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if fi, serr := os.Stat(filepath.Join(*pantryDir, "projects")); serr != nil || !fi.IsDir() {
		fmt.Fprintf(stderr, "error: pantry not found at %q (set -pantry)\n", *pantryDir)
		return 1
	}
	if !*resolveOnly && (*tag == "" || *entrypoint == "") {
		fmt.Fprintln(stderr, "error: -tag and -entrypoint are required unless -resolve-only")
		return 2
	}

	oc, err := newOCIClient("oci://ghcr.io/go-pkgx/packages")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	b := &fsBuilder{oc: oc, pantry: *pantryDir, arch: a, stdout: stdout, stderr: stderr,
		cache: map[string]fsTarball{}}

	rc := b.build(fsRequest{
		rootProj:    rootProj,
		tag:         *tag,
		entrypoint:  *entrypoint,
		resolveOnly: *resolveOnly,
		run:         *run,
		smokeArgs:   smokeArgs,
		keep:        *keep,
	})
	return rc
}

// fsArch holds the per-architecture identifiers the builder needs.
type fsArch struct {
	OARCH  string // OCI architecture: amd64 | arm64
	PARCH  string // pantry platform key: x86-64 | aarch64
	Loader string // glibc loader file name inside the bottle
	Interp string // standard PT_INTERP path the loader is symlinked to
}

// resolveFsArch maps a user-supplied arch token (and aliases) to an fsArch.
func resolveFsArch(a string) (fsArch, error) {
	switch a {
	case "amd64", "x86-64", "x86_64":
		return fsArch{"amd64", "x86-64", "ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2"}, nil
	case "arm64", "aarch64":
		return fsArch{"arm64", "aarch64", "ld-linux-aarch64.so.1", "/lib/ld-linux-aarch64.so.1"}, nil
	default:
		return fsArch{}, fmt.Errorf("unknown arch %q (want amd64|arm64)", a)
	}
}

// --- dependency parsing (port of closure.py:parse_deps) ---------------------

// fsHostRe: a dependency key is a PROJECT iff its first path segment (up to the
// first '/') contains a dot — a host like gnu.org, openssl.org, github.com/o/r.
// Bare words (linux, darwin, aarch64, x86-64) are platform maps.
var fsHostRe = regexp.MustCompile(`^[^/\s]+\.[^/\s]+`)

// isHostKey reports whether a dependency key names a project (vs a platform map).
func isHostKey(k string) bool { return fsHostRe.MatchString(k) }

// parseRuntimeDeps returns the runtime-dependency project names a recipe
// declares in its TOP-LEVEL `dependencies:` that apply to linux/<parch>. It
// mirrors closure.py:parse_deps — build.dependencies (nested under `build:`) is
// excluded because it is not part of the top-level `dependencies:` map, and
// platform-map keys are descended into only for linux and the target arch.
func parseRuntimeDeps(deps map[string]any, parch string) []string {
	platformOK := map[string]bool{"linux": true, parch: true}
	var out []string
	var walk func(m map[string]any, allowed bool)
	walk = func(m map[string]any, allowed bool) {
		// Deterministic order for reproducible closures.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if isHostKey(k) {
				if allowed {
					out = append(out, k)
				}
				continue
			}
			// A non-host key is a platform map; descend only when it applies.
			if sub, ok := m[k].(map[string]any); ok {
				walk(sub, allowed && platformOK[k])
			}
		}
	}
	walk(deps, true)
	return out
}

// recipeDeps loads proj's package.yml from the pantry and returns its top-level
// runtime deps for linux/<parch>. A missing recipe returns (nil, false) — a leaf
// whose bottle is still required but which cannot be recursed into.
func recipeDeps(pantryDir, proj, parch string) ([]string, bool) {
	data, err := os.ReadFile(filepath.Join(pantryDir, "projects", proj, "package.yml"))
	if err != nil {
		return nil, false
	}
	rec, err := pantry.Parse(data)
	if err != nil {
		return nil, false
	}
	return parseRuntimeDeps(rec.Dependencies, parch), true
}

// declaredClosure returns the transitive runtime closure of root in post-order
// (deps before dependents) with a visited-set, gnu.org/glibc forced first. A
// faithful port of closure.py:closure + the glibc-first step.
func (b *fsBuilder) declaredClosure(root string) []string {
	seen := map[string]bool{}
	var order []string
	var visit func(proj string)
	visit = func(proj string) {
		if seen[proj] {
			return
		}
		seen[proj] = true
		deps, ok := recipeDeps(b.pantry, proj, b.arch.PARCH)
		if !ok {
			fmt.Fprintf(b.stderr, "fromscratch: no recipe for %s (leaf)\n", proj)
		}
		for _, d := range deps {
			visit(d)
		}
		order = append(order, proj)
	}
	visit(root)
	out := []string{glibcProject}
	for _, p := range order {
		if p != glibcProject {
			out = append(out, p)
		}
	}
	return out
}

const glibcProject = "gnu.org/glibc"

// --- version selection ------------------------------------------------------

// fsVerRe matches a dotted numeric version tag (rejects sha256-… and other
// non-version tags). Faithful to closure.py:VER_RE.
var fsVerRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// versionKey parses a dotted numeric tag into its integer components.
func versionKey(tag string) []int {
	parts := strings.Split(tag, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}

// compareVersionTags orders two version tags the way Python tuple comparison
// does: element-wise, and on a shared prefix the longer tuple is greater
// (1.2.0 > 1.2). Returns -1, 0 or 1.
func compareVersionTags(a, b string) int {
	ka, kb := versionKey(a), versionKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			if ka[i] < kb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ka) < len(kb):
		return -1
	case len(ka) > len(kb):
		return 1
	default:
		return 0
	}
}

// versionCandidates filters tags to numeric versions, newest first.
func versionCandidates(tags []string) []string {
	var cands []string
	for _, t := range tags {
		if fsVerRe.MatchString(t) {
			cands = append(cands, t)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return compareVersionTags(cands[i], cands[j]) > 0 })
	return cands
}

// --- builder ----------------------------------------------------------------

type fsTarball struct {
	ver  string
	data []byte
	ext  string
}

// newOCIClient constructs the registry client (seam: tests inject a fake).
var newOCIClient = func(base string) (ociPuller, error) { return bottle.NewOCIClient(base) }

// ociPuller is the slice of the OCI registry client the builder needs
// (satisfied by *bottle.OCIClient); an interface so tests can inject a fake.
type ociPuller interface {
	ListTags(project string) ([]string, error)
	Pull(project, ver, osn, arch string) ([]byte, string, error)
}

type fsBuilder struct {
	oc     ociPuller
	pantry string
	arch   fsArch
	stdout io.Writer
	stderr io.Writer
	cache  map[string]fsTarball // proj -> resolved+downloaded winning bottle
}

type fsRequest struct {
	rootProj    string
	tag         string
	entrypoint  string
	resolveOnly bool
	run         bool
	smokeArgs   []string
	keep        bool
}

// resolveBottle returns the newest published linux/<arch> bottle of proj,
// downloading the winning layer (cached for the later extract). ok is false when
// no numeric version carries a linux/<arch> manifest (an unpublished member).
func (b *fsBuilder) resolveBottle(proj string) (fsTarball, bool) {
	if t, done := b.cache[proj]; done {
		return t, t.ver != ""
	}
	tags, err := b.oc.ListTags(proj)
	if err != nil {
		b.cache[proj] = fsTarball{} // negative-cache: unpublished / absent repo
		return fsTarball{}, false
	}
	for _, ver := range versionCandidates(tags) {
		data, ext, err := b.oc.Pull(proj, ver, "linux", b.arch.OARCH)
		if err != nil {
			continue // no linux/<arch> manifest for this version — try older
		}
		t := fsTarball{ver: ver, data: data, ext: ext}
		b.cache[proj] = t
		return t, true
	}
	b.cache[proj] = fsTarball{}
	return fsTarball{}, false
}

// build runs the whole flow and returns a process exit code (0 ok, 1 error,
// 3 = closure not fully published — the contract the CI panel keys on).
func (b *fsBuilder) build(req fsRequest) int {
	// 1. Declared runtime closure (glibc first).
	projects := b.declaredClosure(req.rootProj)

	// 2. Resolve each to its newest published linux/<arch> bottle.
	var missing []string
	versions := map[string]string{}
	for _, p := range projects {
		t, ok := b.resolveBottle(p)
		if !ok {
			missing = append(missing, p)
			continue
		}
		versions[p] = t.ver
	}
	if len(missing) > 0 {
		fmt.Fprintf(b.stderr, "fromscratch: NOT published for linux/%s: %s\n",
			b.arch.OARCH, strings.Join(missing, ", "))
		return 3
	}

	if req.resolveOnly {
		for _, p := range projects {
			fmt.Fprintf(b.stdout, "%s:%s\n", p, versions[p])
		}
		return 0
	}

	rootVer := versions[req.rootProj]

	// 3. Lay out every declared bottle under <work>/root/pkgx/<proj>/v<ver>.
	work, err := fsMkdirTemp("", "fromscratch-")
	if err != nil {
		fmt.Fprintln(b.stderr, "error:", err)
		return 1
	}
	if req.keep {
		fmt.Fprintf(b.stderr, "fromscratch: workdir kept at %s\n", work)
	} else {
		defer os.RemoveAll(work)
	}
	root := filepath.Join(work, "root")
	pkgx := filepath.Join(root, "pkgx")
	if err := os.MkdirAll(pkgx, 0o755); err != nil {
		fmt.Fprintln(b.stderr, "error:", err)
		return 1
	}

	fmt.Fprintf(b.stdout, "fromscratch: %s closure (linux/%s) = %d declared bottle(s)\n",
		req.rootProj, b.arch.OARCH, len(projects))
	for _, p := range projects {
		fmt.Fprintf(b.stdout, "  %s:%s\n", p, versions[p])
		t := b.cache[p]
		if err := extractBottle(t.data, t.ext, pkgx); err != nil {
			fmt.Fprintf(b.stderr, "error: extract %s:%s: %v\n", p, t.ver, err)
			return 1
		}
	}

	// 4. readelf-driven completion: pull providers of undeclared NEEDED sonames.
	if err := b.completeElfClosure(root, pkgx); err != nil {
		fmt.Fprintln(b.stderr, "error:", err)
		return 1
	}

	// 5. Loader symlink → standard PT_INTERP path.
	loaderImg := findLoaderImage(root, b.arch.Loader)
	if loaderImg == "" {
		fmt.Fprintf(b.stderr, "error: glibc loader %s not found in the glibc bottle\n", b.arch.Loader)
		return 1
	}
	if err := symlinkLoader(root, b.arch.Interp, loaderImg); err != nil {
		fmt.Fprintln(b.stderr, "error:", err)
		return 1
	}

	// 6. LD_LIBRARY_PATH from every shared-object dir.
	dirs, err := discoverLibDirs(root)
	if err != nil {
		fmt.Fprintln(b.stderr, "error:", err)
		return 1
	}
	if len(dirs) == 0 {
		fmt.Fprintln(b.stderr, "error: no shared-object dirs discovered (empty LD_LIBRARY_PATH)")
		return 1
	}
	ldPath := strings.Join(dirs, ":")

	// 7. Dockerfile + docker build.
	entry := strings.ReplaceAll(req.entrypoint, "{V}", "v"+rootVer)
	if err := writeDockerfile(work, ldPath, entry); err != nil {
		fmt.Fprintln(b.stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(b.stdout, "fromscratch: building %s (linux/%s)\n", req.tag, b.arch.OARCH)
	fmt.Fprintf(b.stdout, "  LD_LIBRARY_PATH=%s\n", ldPath)
	fmt.Fprintf(b.stdout, "  loader %s -> %s\n", b.arch.Interp, loaderImg)
	fmt.Fprintf(b.stdout, "  ENTRYPOINT %s\n", entry)
	if err := dockerBuild(work, b.arch.OARCH, req.tag, b.stdout, b.stderr); err != nil {
		fmt.Fprintln(b.stderr, "error: docker build:", err)
		return 1
	}
	fmt.Fprintf(b.stdout, "fromscratch: OK — %s\n", req.tag)

	// 8. Optional smoke run (its exit code is the command's exit code).
	if req.run {
		if err := dockerRun(b.arch.OARCH, req.tag, req.smokeArgs, b.stdout, b.stderr); err != nil {
			fmt.Fprintln(b.stderr, "fromscratch: smoke run FAILED:", err)
			return 1
		}
		fmt.Fprintln(b.stdout, "fromscratch: smoke run OK")
	}
	return 0
}

// completeElfClosure repeatedly scans the laid-out root for unsatisfied
// DT_NEEDED sonames and pulls each soname's provider bottle (via the curated
// soname→project map) until no new bottle is added. Unknown sonames are warned
// about but do not fail the build (they may be genuinely absent providers).
func (b *fsBuilder) completeElfClosure(root, pkgx string) error {
	warned := map[string]bool{}
	for round := 0; round < 8; round++ {
		needed, present, err := scanNeeded(root)
		if err != nil {
			return err
		}
		changed := false
		// Deterministic iteration.
		sonames := make([]string, 0, len(needed))
		for s := range needed {
			sonames = append(sonames, s)
		}
		sort.Strings(sonames)
		for _, soname := range sonames {
			if present[soname] || isGlibcSoname(soname) {
				continue
			}
			proj := projectForSoname(soname)
			if proj == "" {
				if !warned[soname] {
					fmt.Fprintf(b.stderr, "fromscratch: unresolved soname %s (no provider) — image may fail at runtime\n", soname)
					warned[soname] = true
				}
				continue
			}
			if _, done := b.cache[proj]; done {
				// Already resolved (present or negative-cached); presence check
				// above governs whether the soname is satisfied.
				if t, ok := b.cache[proj]; ok && t.ver == "" {
					continue // provider unpublished for the arch
				}
			}
			t, ok := b.resolveBottle(proj)
			if !ok {
				if !warned[proj] {
					fmt.Fprintf(b.stderr, "fromscratch: provider %s for %s not published for linux/%s — skipping\n", proj, soname, b.arch.OARCH)
					warned[proj] = true
				}
				continue
			}
			// Only extract a provider once (guard by presence re-scan next round).
			prefix := filepath.Join(pkgx, filepath.FromSlash(proj), "v"+t.ver)
			if _, serr := os.Stat(prefix); serr == nil {
				continue
			}
			if err := extractBottle(t.data, t.ext, pkgx); err != nil {
				return fmt.Errorf("extract provider %s:%s: %w", proj, t.ver, err)
			}
			fmt.Fprintf(b.stderr, "fromscratch: readelf: %s NEEDs %s → pulled %s:%s\n", "closure", soname, proj, t.ver)
			changed = true
		}
		if !changed {
			break
		}
	}
	return nil
}

// --- ELF / layout helpers ---------------------------------------------------

// scanNeeded walks root and returns the set of DT_NEEDED sonames across every
// dynamic ELF, and the set of shared-object base names (and advertised SONAMEs)
// present on disk. Non-ELF files are skipped. Pure Go via debug/elf.
func scanNeeded(root string) (needed, present map[string]bool, err error) {
	needed = map[string]bool{}
	present = map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if strings.Contains(d.Name(), ".so") {
			present[d.Name()] = true
		}
		f, oerr := elf.Open(path)
		if oerr != nil {
			return nil // not an ELF
		}
		defer f.Close()
		if sonames, serr := f.DynString(elf.DT_SONAME); serr == nil {
			for _, s := range sonames {
				present[s] = true
			}
		}
		libs, derr := f.DynString(elf.DT_NEEDED)
		if derr != nil {
			return nil // not dynamic
		}
		for _, l := range libs {
			needed[l] = true
		}
		return nil
	})
	return needed, present, err
}

// discoverLibDirs returns every image-absolute directory under root holding a
// shared object, sorted and de-duplicated — the LD_LIBRARY_PATH. It is a
// function var so build's error/empty branches are testable via a fake.
var discoverLibDirs = func(root string) ([]string, error) {
	set := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".so") || strings.Contains(name, ".so.") {
			set[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var dirs []string
	for dir := range set {
		rel := strings.TrimPrefix(dir, root)
		if rel == "" {
			rel = "/"
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	sort.Strings(dirs)
	return dirs, nil
}

// findLoaderImage returns the image-absolute path of the glibc loader file
// (named loader) inside the laid-out glibc bottle under root, or "". The walk
// callback swallows errors (a missing glibc dir just yields ""), so this never
// itself errors — hence the string-only signature.
func findLoaderImage(root, loader string) string {
	var found string
	base := filepath.Join(root, "pkgx", "gnu.org", "glibc")
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return nil // glibc dir absent → caller reports ""
		}
		if !d.IsDir() && d.Name() == loader {
			if info, ierr := os.Lstat(path); ierr == nil && info.Mode().IsRegular() {
				found = filepath.ToSlash(strings.TrimPrefix(path, root))
				return fs.SkipAll
			}
		}
		return nil
	})
	return found
}

// symlinkLoader creates the standard PT_INTERP symlink (interp) inside root
// pointing at the image-absolute glibc loader path. A function var so build's
// error branch is testable via a fake.
var symlinkLoader = func(root, interp, loaderImg string) error {
	dst := filepath.Join(root, interp)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Symlink(loaderImg, dst)
}

// --- image ------------------------------------------------------------------

// writeDockerfile writes a `FROM scratch` Dockerfile into workdir. A function
// var so build's error branch is testable via a fake.
var writeDockerfile = func(workdir, ldPath, entrypoint string) error {
	content := fmt.Sprintf("FROM scratch\nCOPY root/ /\nENV LD_LIBRARY_PATH=%s\nENTRYPOINT [%q]\n",
		ldPath, entrypoint)
	return os.WriteFile(filepath.Join(workdir, "Dockerfile"), []byte(content), 0o644)
}

// Injectable seams over os/io operations whose error branches are otherwise
// unreachable in a test (a successful syscall that then fails is not
// reproducible with real files). Tests override these; production uses the
// stdlib verbatim.
var (
	fsMkdirTemp = os.MkdirTemp
	ioCopy      = io.Copy
	// openFileWrite returns an io.WriteCloser (not *os.File) so a test can inject
	// a writer whose Close fails.
	openFileWrite = func(name string, perm os.FileMode) (io.WriteCloser, error) {
		return os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	}
	// execCommand is the process seam (tests point it at a helper process).
	execCommand = exec.Command
)

// runDocker execs docker with argv, wiring stdout/stderr.
func runDocker(argv []string, stdout, stderr io.Writer) error {
	cmd := execCommand("docker", argv...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// dockerBuild runs `docker build --platform linux/<oarch>` on workdir.
var dockerBuild = func(workdir, oarch, tag string, stdout, stderr io.Writer) error {
	return runDocker([]string{"build", "--platform", "linux/" + oarch, "-t", tag, workdir}, stdout, stderr)
}

// dockerRun runs `docker run --rm --platform linux/<oarch> <tag> <args…>`.
var dockerRun = func(oarch, tag string, args []string, stdout, stderr io.Writer) error {
	return runDocker(append([]string{"run", "--rm", "--platform", "linux/" + oarch, tag}, args...), stdout, stderr)
}

// --- bottle extraction ------------------------------------------------------

// extractBottle decompresses a bottle tarball (gzip or xz) into destPkgx. Bottle
// entries already carry the "<proj>/v<ver>/…" prefix, so they land directly at
// destPkgx/<proj>/v<ver>/….
func extractBottle(data []byte, ext, destPkgx string) error {
	var r io.Reader = bytes.NewReader(data)
	switch ext {
	case ".tar.xz":
		xr, err := xz.NewReader(r)
		if err != nil {
			return err
		}
		r = xr
	default: // ".tar.gz"
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	return extractTar(tar.NewReader(r), destPkgx)
}

// extractTar writes a tar stream into dest, handling regular files, dirs,
// symlinks and hardlinks; paths are constrained to dest.
func extractTar(tr *tar.Reader, dest string) error {
	cleanDest := filepath.Clean(dest)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe tar path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Remove any existing object first so O_CREATE cannot FOLLOW a stale
			// symlink already at this path (terminfo ships symlink aliases that,
			// on a case-insensitive host FS, collide and loop → ELOOP).
			_ = os.Remove(target)
			f, err := openFileWrite(target, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := ioCopy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(filepath.Join(dest, hdr.Linkname), target); err != nil {
				return err
			}
		}
	}
}
