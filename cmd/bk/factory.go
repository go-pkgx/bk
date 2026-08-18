package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-attest/sign"
	"github.com/go-pkgx/bk/bottlepkg"
	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/overrides"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bk/versions"
	"github.com/go-pkgx/bottle"
)

// defaultEpoch pins SOURCE_DATE_EPOCH when the environment leaves it unset, so
// re-publishing the same bottle produces byte-identical SBOM/provenance — hence
// identical referrer digests, so a re-push is idempotent instead of piling up
// duplicates. (factory.sh pinned the same value.)
const defaultEpoch = 1700000000

// failTailLines is how much of a failed build's output lands in
// failures-detail.txt: container-job logs are NOT retrievable through the GitHub
// API, so that artifact is the only way to read the real error afterwards.
const failTailLines = 30

// Seams: everything the factory does that touches the network, a pantry
// checkout or a real compiler, so runFactory is unit-testable end to end.
var (
	factoryOverrides = overrides.Apply
	factoryList      = versions.List
	factoryResolve   = versions.Resolve
	factoryPublish   = publishBottle
	factoryBuild     = func(r *build.Runner, rec *pantry.Recipe, project, constraint string, tgt, host target.Target, out string) (build.Result, error) {
		return r.Build(rec, project, constraint, tgt, host, out)
	}
	factoryUpstreamVersions = bottle.VersionsFor
	factoryDownload         = bottle.DownloadBottle
	factoryHasPlatform      = func(dist, project, ver, osn, arch string) (bool, error) {
		c, err := bottle.NewOCIClient(dist)
		if err != nil {
			return false, err
		}
		return c.HasPlatform(project, ver, osn, arch)
	}
)

// runFactory implements `bk factory`: build a set of pantry recipes — expanded
// to their full runtime closure, deps first — and publish every resulting bottle
// to an OCI registry, signed and attested. It is the pure-Go replacement for
// packages/factory.sh, and keeps its contract:
//
//   - each REQUESTED project builds EVERY available upstream version (newest
//     first, optionally capped); closure-only dependencies build a single
//     resolved-latest;
//   - a (project, tag, platform) already in the registry is SKIPPED, so shared
//     deps build once and successive runs only fill what is missing;
//   - a per-recipe failure is recorded (failures.txt + failures-detail.txt) and
//     never fails the run: the pantry is built progressively.
func runFactory(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	fs.SetOutput(stderr)
	recipes := fs.String("recipes", envOr("RECIPES", ""), "space-separated projects to build (default: --recipes-file)")
	recipesFile := fs.String("recipes-file", "recipes.txt", "file listing one project per line (# comments allowed)")
	pantryDir := fs.String("pantry", envOr("PANTRY", "pantry"), "pantry checkout to build from")
	overridesDir := fs.String("overrides", "overrides", `directory of *.patch recipe overrides ("" to skip)`)
	to := fs.String("to", envOr("DIST", "oci://ghcr.io/go-pkgx/packages"), "oci:// registry to publish to")
	platform := fs.String("platform", envOr("PLATFORM", ""), "target os/arch, e.g. linux/x86-64 (required)")
	bottles := fs.String("bottles", "dist", "local directory the built bottles are staged in")
	maxVersions := fs.Int("max-versions", envInt("MAX_VERSIONS"), "cap versions built per requested project, newest first (0 = all)")
	versionSpec := fs.String("versions", envOr("VERSIONS", ""), `only consider versions of the REQUESTED projects that match this pkgx constraint, e.g. "^3", ">=2.4", "=1.2.3" (applied before --max-versions; closure-only dependencies still resolve to their newest)`)
	mirrorFrom := fs.String("mirror-from", "", "instead of building, copy each bottle from this upstream pkgx dist (e.g. https://dist.pkgx.dev) and republish it signed + attested — for versions we cannot or need not rebuild, such as ancient glibc")
	libc := fs.String("libc", "", `C library to link against: "pkgx" targets the gnu.org/glibc bottle instead of the build container's`)
	glibc := fs.String("glibc", "", "build and publish the whole closure against this exact glibc, e.g. 2.27.0 (implies --libc=pkgx)")
	force := fs.Bool("force", os.Getenv("FORCE") != "", "rebuild and republish even when the bottle is already in the registry")
	compress := fs.String("compress", envOr("COMPRESS", "zstd"), "codec for NEW bottles: zstd or gzip. Already-published bottles are never rewritten, so gzip stays readable; this only governs what we create")
	signKey := fs.String("sign", "", "sign published bottles with this go-attest/sign secret key file (else $SIGNING_KEY)")
	pkgx := fs.String("pkgx", "pkgx", "path to the pkgx binary used for the deps env")
	failures := fs.String("failures", "failures.txt", "write the list of failed builds here")
	failuresDetail := fs.String("failures-detail", "failures-detail.txt", "write each failure's error tail here")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *platform == "" {
		fmt.Fprintln(stderr, "usage: bk factory --platform os/arch [--recipes \"p1 p2\"] [--pantry dir] [--to oci://…]")
		return 2
	}
	osn, arch, ok := splitPlatform(*platform)
	if !ok {
		fmt.Fprintf(stderr, "factory: bad --platform %q (want os/arch)\n", *platform)
		return 2
	}
	// target.Resolve reads BREWKIT_TARGET, and needs it set to get the full
	// target (triple included) for the requested platform.
	os.Setenv("BREWKIT_TARGET", *platform)
	tgt, err := target.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "factory:", err)
		return 1
	}

	// --glibc is the whole-stack glibc floor: it only means anything when the
	// build links the pkgx glibc bottle rather than the container's.
	if *glibc != "" && *libc == "" {
		*libc = "pkgx"
	}

	kp, err := factorySigningKey(*signKey)
	if err != nil {
		fmt.Fprintln(stderr, "factory:", err)
		return 1
	}
	fmt.Fprintln(stdout, "signing:", map[bool]string{true: "enabled", false: "disabled (no key)"}[kp != nil])

	// Local recipe overrides: candidate fixes for genuine upstream-recipe bugs,
	// applied to the pantry before the closure is computed so we validate them
	// here before proposing them upstream.
	if *overridesDir != "" {
		if _, err := factoryOverrides(overrides.Options{
			Dir:  *overridesDir,
			Root: *pantryDir,
			Log:  func(s string) { fmt.Fprintln(stdout, s) },
			Warn: func(s string) { fmt.Fprintln(stderr, s) },
		}); err != nil {
			fmt.Fprintln(stderr, "factory:", err)
			return 1
		}
	}

	want, err := factoryWant(*recipes, *recipesFile)
	if err != nil {
		fmt.Fprintln(stderr, "factory:", err)
		return 1
	}
	if len(want) == 0 {
		fmt.Fprintln(stderr, "factory: nothing to build (empty --recipes and --recipes-file)")
		return 1
	}
	requested := map[string]bool{}
	for _, p := range want {
		requested[p] = true
	}

	list := closureOf(*pantryDir, tgt, want, func(s string) { fmt.Fprintln(stderr, s) })
	if *mirrorFrom != "" {
		// Mirroring needs no recipe (no build, and the versions come from the
		// upstream dist), so a requested project the closure walk had to drop for
		// want of one is still mirrored.
		list = appendMissing(list, want)
	}
	fmt.Fprintf(stdout, "closure: %d project(s) for %s (from %d requested)\n", len(list), *platform, len(want))

	pkgxBin := *pkgx
	if abs, err := lookPath(pkgxBin); err == nil {
		pkgxBin = abs
	}
	runner := buildFactory(pkgxBin)
	runner.LibcMode = *libc
	runner.Glibc = *glibc

	if err := setCodec(*compress); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}

	f := &factory{
		runner: runner, tgt: tgt, host: target.Host(),
		osn: osn, arch: arch, platform: *platform,
		dist: *to, bottles: *bottles, glibc: *glibc,
		force: *force, key: kp, when: factoryTime(),
		mirror: strings.TrimRight(*mirrorFrom, "/"),
		want:   strings.TrimSpace(*versionSpec),
		stdout: stdout, stderr: stderr,
	}
	if f.mirror != "" {
		setUpstreamDist(f.mirror)
		fmt.Fprintf(stdout, "mirror: copying bottles from %s (no build)\n", f.mirror)
	}

	for _, proj := range list {
		if f.mirror != "" {
			vers, err := f.mirrorVersionsFor(proj, requested[proj], *maxVersions)
			if err != nil {
				f.fail(proj, "", "versions", err)
				continue
			}
			for _, v := range vers {
				f.mirrorOne(proj, v)
			}
			continue
		}
		recPath := filepath.Join(*pantryDir, "projects", proj, "package.yml")
		data, err := os.ReadFile(recPath)
		if err != nil {
			fmt.Fprintf(stdout, "SKIP %s (no recipe)\n", proj)
			continue
		}
		rec, err := pantry.Parse(data)
		if err != nil {
			f.fail(proj, "", "recipe", err)
			continue
		}
		vers, err := f.versionsFor(rec, proj, requested[proj], *maxVersions)
		if err != nil {
			f.fail(proj, "", "versions", err)
			continue
		}
		runner.RecipeDir = filepath.Dir(recPath)
		for _, v := range vers {
			f.buildOne(rec, proj, v)
		}
	}

	if err := os.WriteFile(*failures, f.failures.Bytes(), 0o644); err != nil {
		fmt.Fprintln(stderr, "factory:", err)
	}
	if err := os.WriteFile(*failuresDetail, f.failuresDetail.Bytes(), 0o644); err != nil {
		fmt.Fprintln(stderr, "factory:", err)
	}
	// Once the batch is over the other publishers have finished too, so a
	// repair sticks — which is the whole reason this runs here rather than
	// after each push.
	if n := repairIndexes(f.dist, f.pushed, stdout, stderr); n > 0 {
		fmt.Fprintf(stdout, "=== %d index(es) repaired after a concurrent publisher dropped a platform ===\n", n)
	}
	fmt.Fprintf(stdout, "=== summary (%s): %d built, %d skipped, %d failed ===\n", *platform, f.ok, f.skipped, f.failed)
	if f.failures.Len() > 0 {
		fmt.Fprint(stdout, "failures:\n", f.failures.String())
	}
	// Per-recipe failures never fail the run: the pantry fills progressively.
	return 0
}

// factory carries one run's configuration and tallies.
type factory struct {
	runner   *build.Runner
	tgt      target.Target
	host     target.Target
	osn      string
	arch     string
	platform string
	dist     string
	bottles  string
	glibc    string
	force    bool
	key      *sign.Keypair
	when     time.Time
	mirror   string // upstream dist to copy from, "" = build
	want     string // pkgx constraint the requested projects' versions must satisfy, "" = any
	stdout   io.Writer
	stderr   io.Writer

	ok, skipped, failed int
	failures            bytes.Buffer
	failuresDetail      bytes.Buffer

	// pushed is what this run put in the registry, walked once at the end to
	// check no concurrent publisher dropped a platform from an index.
	pushed []published
}

// versionsFor lists the versions to build for a project: every candidate
// (newest first, capped by max) when it was explicitly requested, or the single
// resolved latest for a closure-only dependency. Resolving the dependency's
// version here (rather than letting the build resolve it) is what makes the
// skip-if-published check possible for deps too.
func (f *factory) versionsFor(rec *pantry.Recipe, proj string, requested bool, max int) ([]string, error) {
	if !requested {
		v, _, err := factoryResolve(rec.Versions, "*")
		if err != nil {
			return nil, err
		}
		return []string{v}, nil
	}
	vts, err := factoryList(rec.Versions)
	if err != nil {
		return nil, err
	}
	if len(vts) == 0 {
		return nil, fmt.Errorf("no candidate versions")
	}
	out := make([]string, 0, len(vts))
	for _, vt := range vts {
		out = append(out, vt.Version)
	}
	if out = f.matching(proj, out); len(out) == 0 {
		return nil, fmt.Errorf("no version matches %q", f.want)
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	fmt.Fprintf(f.stdout, "versions %s: %d to consider (%s)\n", proj, len(out), f.platform)
	return out, nil
}

// buildOne builds and publishes one (project, version), unless the registry
// already has that bottle for this platform.
func (f *factory) buildOne(rec *pantry.Recipe, proj, ver string) {
	tag := flavoredTag(proj, ver, f.glibc)
	if !f.force {
		switch published, err := factoryHasPlatform(f.dist, proj, tag, f.osn, f.arch); {
		case err != nil:
			// Treat an unreachable registry as "not published" and build: a
			// redundant rebuild is far cheaper than silently skipping the world.
			fmt.Fprintf(f.stderr, "factory: publish-check %s %s: %v\n", proj, tag, err)
		case published:
			fmt.Fprintf(f.stdout, "⏭  SKIP %s %s (%s) — already published\n", proj, tag, f.platform)
			f.skipped++
			return
		}
	}

	fmt.Fprintf(f.stdout, "::group::build %s %s (%s)\n", proj, ver, f.platform)
	tw := &tailWriter{w: f.stdout, max: failTailLines}
	f.runner.Run = runBashTo(tw, tw)
	res, err := factoryBuild(f.runner, rec, proj, "="+ver, f.tgt, f.host, f.bottles)
	fmt.Fprintln(f.stdout, "::endgroup::")
	if err != nil {
		f.failDetail(proj, ver, "build", err, tw.tail())
		return
	}
	if res.BottlePath == "" {
		f.fail(proj, ver, "package", fmt.Errorf("build produced no bottle"))
		return
	}

	// res.Version is what actually got built (the recipe's resolver may
	// normalise the requested version), and is what the attestations record.
	tag, desc, err := factoryPublish(publishOptions{
		Dist: f.dist, Project: proj, Version: res.Version,
		OS: f.osn, Arch: f.arch, Path: res.BottlePath,
		Glibc: f.glibc, Key: f.key, Time: f.when,
	})
	if err != nil {
		f.fail(proj, res.Version, "publish", err)
		return
	}
	fmt.Fprintf(f.stdout, "✅ OK %s %s %s\n", proj, flavoredTag(proj, res.Version, f.glibc), f.platform)
	f.ok++
	f.pushed = append(f.pushed, published{project: proj, tag: tag, desc: desc})
}

// mirrorVersionsFor lists the versions to copy for a project: what the UPSTREAM
// dist actually carries for this platform (newest first, capped) for a requested
// project, or just its newest for a closure-only dependency. Upstream's listing
// — not the recipe's tags — is the truth here: only a version upstream published
// a bottle for can be copied.
func (f *factory) mirrorVersionsFor(proj string, requested bool, max int) ([]string, error) {
	vs, err := factoryUpstreamVersions(proj, f.osn, f.arch)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("no upstream bottle for %s/%s", f.osn, f.arch)
	}
	out := make([]string, 0, len(vs))
	for i := len(vs) - 1; i >= 0; i-- { // upstream lists ascending
		out = append(out, vs[i].Raw)
	}
	if !requested {
		return out[:1], nil
	}
	if out = f.matching(proj, out); len(out) == 0 {
		return nil, fmt.Errorf("no upstream version matches %q", f.want)
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	fmt.Fprintf(f.stdout, "versions %s: %d to consider (%s, upstream)\n", proj, len(out), f.platform)
	return out, nil
}

// matching keeps only the versions satisfying --versions, and says how many it
// dropped. It exists because "newest N" is the wrong handle for a LINE: our
// registry carries cmake 4.4.2 while 114 pantry recipes pin `cmake.org: ^3`, and
// no value of --max-versions reaches a 3.x from a newest-first listing. The
// constraint grammar is pkgx's own, so `^3`, `~3.31`, `>=2.4` and `=1.2.3` all
// mean here what they mean in a recipe.
func (f *factory) matching(proj string, vers []string) []string {
	if f.want == "" {
		return vers
	}
	out := make([]string, 0, len(vers))
	for _, v := range vers {
		if bottle.ParseVer(v).Satisfies(f.want) {
			out = append(out, v)
		}
	}
	if n := len(vers) - len(out); n > 0 {
		fmt.Fprintf(f.stdout, "versions %s: %d dropped by --versions %q\n", proj, n, f.want)
	}
	return out
}

// mirrorOne copies one upstream bottle into our registry, republished with our
// own SBOM, provenance and signature (and, for glibc, its min-kernel floor).
func (f *factory) mirrorOne(proj, ver string) {
	tag := flavoredTag(proj, ver, f.glibc)
	if !f.force {
		switch published, err := factoryHasPlatform(f.dist, proj, tag, f.osn, f.arch); {
		case err != nil:
			fmt.Fprintf(f.stderr, "factory: publish-check %s %s: %v\n", proj, tag, err)
		case published:
			fmt.Fprintf(f.stdout, "⏭  SKIP %s %s (%s) — already published\n", proj, tag, f.platform)
			f.skipped++
			return
		}
	}
	data, ext, err := factoryDownload(proj, ver, f.osn, f.arch)
	if err != nil {
		f.fail(proj, ver, "fetch", err)
		return
	}
	// Stage it in the same pkgx dist layout a build writes, so the Pages mirror
	// picks a mirrored bottle up exactly like a built one.
	dir := filepath.Join(f.bottles, filepath.FromSlash(proj), f.osn, f.arch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.fail(proj, ver, "fetch", err)
		return
	}
	path := filepath.Join(dir, "v"+ver+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		f.fail(proj, ver, "fetch", err)
		return
	}
	tag, desc, err := factoryPublish(publishOptions{
		Dist: f.dist, Project: proj, Version: ver,
		OS: f.osn, Arch: f.arch, Path: path,
		Glibc: f.glibc, Key: f.key, Time: f.when,
	})
	if err != nil {
		f.fail(proj, ver, "publish", err)
		return
	}
	fmt.Fprintf(f.stdout, "✅ MIRRORED %s %s %s (%d KiB)\n", proj, tag, f.platform, len(data)/1024)
	f.ok++
	f.pushed = append(f.pushed, published{project: proj, tag: tag, desc: desc})
}

// appendMissing returns list plus every want not already in it, order preserved.
func appendMissing(list, want []string) []string {
	have := map[string]bool{}
	for _, p := range list {
		have[p] = true
	}
	for _, p := range want {
		if !have[p] {
			have[p] = true
			list = append(list, p)
		}
	}
	return list
}

// setUpstreamDist points the bottle package at the dist being mirrored. Upstream
// static dists carry no signatures (attestations are OCI referrers), and bottle
// verifies by default, so verification is turned off for the copy unless the
// operator demanded it — mirroring copies bytes, it does not install them, and
// what we publish is re-signed with OUR key.
func setUpstreamDist(from string) {
	bottle.DistBase = from
	if _, ok := os.LookupEnv("PKGX_VERIFY"); !ok {
		os.Setenv("PKGX_VERIFY", "0")
	}
}

// fail records a failed (project, version) at a stage.
func (f *factory) fail(proj, ver, stage string, err error) {
	f.failDetail(proj, ver, stage, err, err.Error())
}

// failDetail records a failure plus the text a human needs to diagnose it.
func (f *factory) failDetail(proj, ver, stage string, err error, detail string) {
	if ver == "" {
		ver = "latest"
	}
	fmt.Fprintf(f.stdout, "❌ %s FAIL %s %s: %v\n", strings.ToUpper(stage), proj, ver, err)
	fmt.Fprintf(&f.failures, "%s %s %s\n", proj, ver, stage)
	fmt.Fprintf(&f.failuresDetail, "########## %s %s (%s) — %s failed: %v\n%s\n\n", proj, ver, f.platform, stage, err, detail)
	f.failed++
}

// factoryWant is the requested project list: the --recipes words if given, else
// the non-blank, non-comment lines of --recipes-file.
func factoryWant(recipes, file string) ([]string, error) {
	if strings.TrimSpace(recipes) != "" {
		return strings.Fields(recipes), nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	return out, nil
}

// factorySigningKey loads the bottle signing key from a file, or — as CI has it
// — straight from $SIGNING_KEY, so no secret is ever staged on disk.
func factorySigningKey(path string) (*sign.Keypair, error) {
	if path != "" {
		return loadSigningKey(path)
	}
	if k := os.Getenv("SIGNING_KEY"); k != "" {
		return sign.LoadSecretKey(k)
	}
	return nil, nil
}

// factoryTime is the attestation timestamp: SOURCE_DATE_EPOCH when set, else
// the factory's pinned epoch (never the wall clock, which would make every
// re-publish a new artifact).
func factoryTime() time.Time {
	if os.Getenv("SOURCE_DATE_EPOCH") == "" {
		return time.Unix(defaultEpoch, 0).UTC()
	}
	return buildTime()
}

// envInt reads a non-negative integer environment variable, 0 if unset/invalid.
func envInt(key string) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// tailWriter passes a build's output straight through while remembering its
// last max lines, so a failure's error tail can be written to the artifact CI
// leaves behind.
type tailWriter struct {
	w     io.Writer
	max   int
	part  []byte
	lines []string
}

func (t *tailWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.part = append(t.part, p[:n]...)
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		t.push(string(t.part[:i]))
		t.part = append(t.part[:0], t.part[i+1:]...)
	}
	return n, err
}

func (t *tailWriter) push(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

// tail is the remembered output, trailing partial line included.
func (t *tailWriter) tail() string {
	lines := t.lines
	if len(t.part) > 0 {
		lines = append(append([]string{}, lines...), string(t.part))
	}
	return strings.Join(lines, "\n")
}

// setCodec maps the --compress flag onto the extension bottlepkg writes with.
// Named codecs rather than extensions on the CLI: "zstd" is what an operator
// knows the format as, ".tar.zst" is how it lands on disk.
func setCodec(name string) error {
	switch name {
	case "gzip", "gz":
		bottlepkg.Codec = bottle.ExtTarGz
	case "zstd", "zst":
		bottlepkg.Codec = bottle.ExtTarZst
	default:
		return fmt.Errorf("unknown --compress %q (gzip or zstd)", name)
	}
	return nil
}
