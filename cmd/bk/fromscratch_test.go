package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveFsArch(t *testing.T) {
	cases := map[string]struct {
		oarch, parch, loader, interp string
		ok                           bool
	}{
		"amd64":   {"amd64", "x86-64", "ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2", true},
		"x86-64":  {"amd64", "x86-64", "ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2", true},
		"x86_64":  {"amd64", "x86-64", "ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2", true},
		"arm64":   {"arm64", "aarch64", "ld-linux-aarch64.so.1", "/lib/ld-linux-aarch64.so.1", true},
		"aarch64": {"arm64", "aarch64", "ld-linux-aarch64.so.1", "/lib/ld-linux-aarch64.so.1", true},
		"riscv64": {"", "", "", "", false},
	}
	for in, want := range cases {
		got, err := resolveFsArch(in)
		if want.ok != (err == nil) {
			t.Fatalf("%s: ok=%v err=%v", in, want.ok, err)
		}
		if !want.ok {
			continue
		}
		if got.OARCH != want.oarch || got.PARCH != want.parch || got.Loader != want.loader || got.Interp != want.interp {
			t.Errorf("%s: got %+v", in, got)
		}
	}
}

func TestIsHostKey(t *testing.T) {
	hosts := []string{"gnu.org", "openssl.org", "github.com/besser82/libxcrypt", "invisible-island.net/ncurses"}
	nonHosts := []string{"linux", "darwin", "aarch64", "x86-64", "windows"}
	for _, h := range hosts {
		if !isHostKey(h) {
			t.Errorf("expected host: %q", h)
		}
	}
	for _, n := range nonHosts {
		if isHostKey(n) {
			t.Errorf("expected non-host: %q", n)
		}
	}
}

func TestParseRuntimeDeps(t *testing.T) {
	deps := map[string]any{
		"zlib.net":         "1",
		"gnu.org/readline": "8",
		"linux": map[string]any{
			"invisible-island.net/ncurses": "*",
			"aarch64": map[string]any{
				"only.arm64.dep": "1",
			},
			"x86-64": map[string]any{
				"only.amd64.dep": "1",
			},
		},
		"darwin": map[string]any{
			"only.darwin.dep": "1",
		},
	}
	got := parseRuntimeDeps(deps, "aarch64")
	want := map[string]bool{
		"zlib.net": true, "gnu.org/readline": true,
		"invisible-island.net/ncurses": true, "only.arm64.dep": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected dep %q (got %v)", g, got)
		}
	}
	// x86-64 branch: swap the target arch → only.amd64.dep appears, arm64 dropped.
	got2 := parseRuntimeDeps(deps, "x86-64")
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has(got2, "only.amd64.dep") || has(got2, "only.arm64.dep") || has(got2, "only.darwin.dep") {
		t.Errorf("x86-64 target wrong: %v", got2)
	}
}

func TestVersionOrdering(t *testing.T) {
	if compareVersionTags("1.10.0", "1.9.0") <= 0 {
		t.Error("1.10.0 should be > 1.9.0 (numeric, not lexical)")
	}
	if compareVersionTags("1.2.0", "1.2") <= 0 {
		t.Error("1.2.0 should be > 1.2 (longer prefix wins)")
	}
	if compareVersionTags("1.2", "1.2.0") >= 0 {
		t.Error("1.2 should be < 1.2.0")
	}
	if compareVersionTags("2.0", "2.0") != 0 {
		t.Error("equal versions")
	}
	tags := []string{"1.9.0", "sha256-abcd", "1.10.0", "2.44", "latest", "0.5"}
	got := versionCandidates(tags)
	want := []string{"2.44", "1.10.0", "1.9.0", "0.5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("versionCandidates = %v, want %v", got, want)
	}
}

func TestSonameMap(t *testing.T) {
	cases := map[string]string{
		"libcrypt.so.1":      "github.com/besser82/libxcrypt", // THE readelf-driven case
		"libz.so.1":          "zlib.net",
		"libtinfow.so.6":     "invisible-island.net/ncurses",
		"libreadline.so.8":   "gnu.org/readline",
		"libabsl_foo.so.2":   "abseil.io", // prefix map
		"libboost_regex.so":  "boost.org", // prefix map
		"libc.so.6":          "",          // glibc: not in the map
		"libunknownxyz.so.1": "",          // unknown → no provider
	}
	for soname, want := range cases {
		if got := projectForSoname(soname); got != want {
			t.Errorf("projectForSoname(%q) = %q, want %q", soname, got, want)
		}
	}
	if sonameBase("libz.so.1") != "libz" {
		t.Error("sonameBase libz")
	}
	if sonameBase("noext") != "noext" {
		t.Error("sonameBase no .so")
	}
}

func TestIsGlibcSoname(t *testing.T) {
	yes := []string{"ld-linux-aarch64.so.1", "libc.so.6", "libm.so.6", "libpthread.so.0", "libnss_files.so.2"}
	no := []string{"libcrypt.so.1", "libz.so.1", "libfoo.so.9"}
	for _, s := range yes {
		if !isGlibcSoname(s) {
			t.Errorf("expected glibc soname: %q", s)
		}
	}
	for _, s := range no {
		if isGlibcSoname(s) {
			t.Errorf("expected non-glibc: %q", s)
		}
	}
}

// makeTarGz builds a gzip'd tar from a set of entries for extraction tests.
type tarEntry struct {
	name     string
	typ      byte
	body     string
	linkname string
	mode     int64
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Linkname: e.linkname}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBottle(t *testing.T) {
	dest := t.TempDir()
	data := makeTarGz(t, []tarEntry{
		{name: "proj/v1/", typ: tar.TypeDir},
		{name: "proj/v1/bin/", typ: tar.TypeDir},
		{name: "proj/v1/bin/tool", typ: tar.TypeReg, body: "ELF", mode: 0o755},
		{name: "proj/v1/bin/alias", typ: tar.TypeSymlink, linkname: "tool"},
		{name: "proj/v1/bin/hard", typ: tar.TypeLink, linkname: "proj/v1/bin/tool"},
	})
	if err := extractBottle(data, ".tar.gz", dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "proj/v1/bin/tool")); err != nil || string(b) != "ELF" {
		t.Fatalf("tool: %v %q", err, b)
	}
	if _, err := os.Lstat(filepath.Join(dest, "proj/v1/bin/alias")); err != nil {
		t.Fatalf("alias symlink missing: %v", err)
	}
	// Re-extract over an existing symlink at a regular-file path must not ELOOP.
	if err := extractBottle(data, ".tar.gz", dest); err != nil {
		t.Fatalf("re-extract: %v", err)
	}
}

func TestExtractTarUnsafePath(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	_, _ = tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	if err := extractBottle(buf.Bytes(), ".tar.gz", t.TempDir()); err == nil {
		t.Fatal("expected unsafe-path error")
	}
}

func TestExtractBottleBadGzip(t *testing.T) {
	if err := extractBottle([]byte("not gzip"), ".tar.gz", t.TempDir()); err == nil {
		t.Fatal("expected gzip error")
	}
	if err := extractBottle([]byte("not xz"), ".tar.xz", t.TempDir()); err == nil {
		t.Fatal("expected xz error")
	}
}

func TestScanNeededAndLibDirs(t *testing.T) {
	root := t.TempDir()
	libdir := filepath.Join(root, "pkgx", "p", "v1", "lib")
	if err := os.MkdirAll(libdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-ELF file named like a shared object → counted present, skipped as ELF.
	if err := os.WriteFile(filepath.Join(libdir, "libz.so.1"), []byte("not-elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libdir, "plain.so"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	needed, present, err := scanNeeded(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(needed) != 0 {
		t.Errorf("no ELF → no NEEDED, got %v", needed)
	}
	if !present["libz.so.1"] {
		t.Errorf("present should include libz.so.1: %v", present)
	}
	dirs, err := discoverLibDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "/pkgx/p/v1/lib" {
		t.Errorf("discoverLibDirs = %v", dirs)
	}
}

func TestFindLoaderAndSymlink(t *testing.T) {
	root := t.TempDir()
	gdir := filepath.Join(root, "pkgx", "gnu.org", "glibc", "v2.44", "lib", "glibc-2.44")
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, "ld-linux-aarch64.so.1"), []byte("loader"), 0o755); err != nil {
		t.Fatal(err)
	}
	img, err := findLoaderImage(root, "ld-linux-aarch64.so.1")
	if err != nil {
		t.Fatal(err)
	}
	if img != "/pkgx/gnu.org/glibc/v2.44/lib/glibc-2.44/ld-linux-aarch64.so.1" {
		t.Fatalf("loader img = %q", img)
	}
	if err := symlinkLoader(root, "/lib/ld-linux-aarch64.so.1", img); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(root, "lib", "ld-linux-aarch64.so.1"))
	if err != nil || got != img {
		t.Fatalf("readlink = %q, %v", got, err)
	}
	// Missing loader → "".
	if img2, _ := findLoaderImage(root, "ld-linux-x86-64.so.2"); img2 != "" {
		t.Errorf("expected empty for absent loader, got %q", img2)
	}
}

func TestWriteDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := writeDockerfile(dir, "/a:/b", "/pkgx/p/v1/bin/x"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM scratch\nCOPY root/ /\nENV LD_LIBRARY_PATH=/a:/b\nENTRYPOINT [\"/pkgx/p/v1/bin/x\"]\n"
	if string(b) != want {
		t.Fatalf("dockerfile:\n%q", b)
	}
}

func TestRecipeDepsAndClosure(t *testing.T) {
	pantry := t.TempDir()
	write := func(proj, yml string) {
		dir := filepath.Join(pantry, "projects", proj)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// root depends on dep.org; dep.org has only build.dependencies (excluded).
	write("root.org", "distributable:\n  url: https://x/{{version}}.tgz\nversions:\n  - 1.0\ndependencies:\n  dep.org: '*'\nbuild:\n  script: make\n")
	write("dep.org", "distributable:\n  url: https://x/{{version}}.tgz\nversions:\n  - 1.0\nbuild:\n  dependencies:\n    gnu.org/make: '*'\n  script: make\n")

	deps, ok := recipeDeps(pantry, "root.org", "aarch64")
	if !ok || len(deps) != 1 || deps[0] != "dep.org" {
		t.Fatalf("root deps = %v ok=%v", deps, ok)
	}
	deps2, ok := recipeDeps(pantry, "dep.org", "aarch64")
	if !ok || len(deps2) != 0 {
		t.Fatalf("dep.org should have no runtime deps, got %v", deps2)
	}
	if _, ok := recipeDeps(pantry, "missing.org", "aarch64"); ok {
		t.Error("missing recipe should return ok=false")
	}

	b := &fsBuilder{pantry: pantry, arch: fsArch{PARCH: "aarch64"}, stderr: &bytes.Buffer{}}
	order := b.declaredClosure("root.org")
	// glibc first, then post-order deps before dependents.
	if order[0] != glibcProject {
		t.Fatalf("glibc must be first: %v", order)
	}
	idxDep, idxRoot := indexOf(order, "dep.org"), indexOf(order, "root.org")
	if idxDep < 0 || idxRoot < 0 || idxDep > idxRoot {
		t.Fatalf("dep must precede root: %v", order)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// fakeOCI is an in-memory ociPuller for build-flow tests.
type fakeOCI struct {
	tags    map[string][]string // project -> tags
	bottles map[string][]byte   // "proj@ver" -> gz tarball
	tagsErr map[string]bool     // project -> ListTags error (repo absent)
}

func (f *fakeOCI) ListTags(project string) ([]string, error) {
	if f.tagsErr[project] {
		return nil, fmt.Errorf("name unknown")
	}
	tags, ok := f.tags[project]
	if !ok {
		return nil, fmt.Errorf("name unknown")
	}
	return tags, nil
}

func (f *fakeOCI) Pull(project, ver, osn, arch string) ([]byte, string, error) {
	if b, ok := f.bottles[project+"@"+ver]; ok {
		return b, ".tar.gz", nil
	}
	return nil, "", fmt.Errorf("no bottle for %s v%s (%s/%s)", project, ver, osn, arch)
}

func TestBuildResolveOnly(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "root.org", "dependencies:\n  gnu.org/glibc: '*'\n")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	f := &fakeOCI{
		tags: map[string][]string{
			"root.org":      {"1.0", "0.9"},
			"gnu.org/glibc": {"2.44"},
		},
		bottles: map[string][]byte{
			"root.org@1.0":       makeTarGz(t, []tarEntry{{name: "root.org/v1.0/", typ: tar.TypeDir}}),
			"gnu.org/glibc@2.44": makeTarGz(t, []tarEntry{{name: "gnu.org/glibc/v2.44/", typ: tar.TypeDir}}),
		},
	}
	var out, errb bytes.Buffer
	b := &fsBuilder{oc: f, pantry: pantry, arch: fsArch{OARCH: "arm64", PARCH: "aarch64"},
		stdout: &out, stderr: &errb, cache: map[string]fsTarball{}}
	rc := b.build(fsRequest{rootProj: "root.org", resolveOnly: true})
	if rc != 0 {
		t.Fatalf("rc=%d err=%s", rc, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "gnu.org/glibc:2.44") || !strings.Contains(got, "root.org:1.0") {
		t.Fatalf("resolve-only output:\n%s", got)
	}
}

func TestBuildUnpublished(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "root.org", "")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	f := &fakeOCI{
		tags:    map[string][]string{"gnu.org/glibc": {"2.44"}},
		tagsErr: map[string]bool{"root.org": true}, // unpublished
		bottles: map[string][]byte{"gnu.org/glibc@2.44": makeTarGz(t, []tarEntry{{name: "gnu.org/glibc/v2.44/", typ: tar.TypeDir}})},
	}
	var out, errb bytes.Buffer
	b := &fsBuilder{oc: f, pantry: pantry, arch: fsArch{OARCH: "arm64", PARCH: "aarch64"},
		stdout: &out, stderr: &errb, cache: map[string]fsTarball{}}
	rc := b.build(fsRequest{rootProj: "root.org", resolveOnly: true})
	if rc != 3 {
		t.Fatalf("expected exit 3 for unpublished closure, got %d", rc)
	}
	if !strings.Contains(errb.String(), "NOT published") {
		t.Errorf("stderr: %s", errb.String())
	}
}

func TestBuildFullFlowWithFakes(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "lz4.org", "")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	// glibc bottle carries the loader file; lz4 carries a .so so LD path is non-empty.
	glibcBottle := makeTarGz(t, []tarEntry{
		{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/", typ: tar.TypeDir},
		{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/ld-linux-aarch64.so.1", typ: tar.TypeReg, body: "L", mode: 0o755},
		{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/libc.so.6", typ: tar.TypeReg, body: "C"},
	})
	lz4Bottle := makeTarGz(t, []tarEntry{
		{name: "lz4.org/v1.10.0/bin/", typ: tar.TypeDir},
		{name: "lz4.org/v1.10.0/bin/lz4", typ: tar.TypeReg, body: "bin", mode: 0o755},
		{name: "lz4.org/v1.10.0/lib/liblz4.so.1", typ: tar.TypeReg, body: "so"},
	})
	f := &fakeOCI{
		tags:    map[string][]string{"lz4.org": {"1.10.0"}, "gnu.org/glibc": {"2.44"}},
		bottles: map[string][]byte{"lz4.org@1.10.0": lz4Bottle, "gnu.org/glibc@2.44": glibcBottle},
	}
	// Seam docker.
	var built, ran bool
	origBuild, origRun := dockerBuild, dockerRun
	defer func() { dockerBuild, dockerRun = origBuild, origRun }()
	dockerBuild = func(workdir, oarch, tag string, _, _ io.Writer) error {
		built = true
		// Dockerfile must exist.
		if _, err := os.Stat(filepath.Join(workdir, "Dockerfile")); err != nil {
			return err
		}
		return nil
	}
	dockerRun = func(oarch, tag string, args []string, _, _ io.Writer) error {
		ran = true
		return nil
	}
	var out, errb bytes.Buffer
	b := &fsBuilder{oc: f, pantry: pantry, arch: fsArch{OARCH: "arm64", PARCH: "aarch64", Loader: "ld-linux-aarch64.so.1", Interp: "/lib/ld-linux-aarch64.so.1"},
		stdout: &out, stderr: &errb, cache: map[string]fsTarball{}}
	rc := b.build(fsRequest{rootProj: "lz4.org", tag: "img", entrypoint: "/pkgx/lz4.org/{V}/bin/lz4", run: true, smokeArgs: []string{"--version"}})
	if rc != 0 {
		t.Fatalf("rc=%d err=%s", rc, errb.String())
	}
	if !built || !ran {
		t.Fatalf("built=%v ran=%v", built, ran)
	}
	if !strings.Contains(out.String(), "ENTRYPOINT /pkgx/lz4.org/v1.10.0/bin/lz4") {
		t.Errorf("entrypoint {V} not expanded:\n%s", out.String())
	}
}

func TestRunFromscratchCLI(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "root.org", "")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	fake := &fakeOCI{
		tags: map[string][]string{"root.org": {"1.0"}, "gnu.org/glibc": {"2.44"}},
		bottles: map[string][]byte{
			"root.org@1.0":       makeTarGz(t, []tarEntry{{name: "root.org/v1.0/", typ: tar.TypeDir}}),
			"gnu.org/glibc@2.44": makeTarGz(t, []tarEntry{{name: "gnu.org/glibc/v2.44/", typ: tar.TypeDir}}),
		},
	}
	orig := newOCIClient
	newOCIClient = func(string) (ociPuller, error) { return fake, nil }
	defer func() { newOCIClient = orig }()

	if rc := runFromscratch([]string{"root.org"}, io.Discard, io.Discard); rc != 2 {
		t.Errorf("missing arch: rc=%d", rc)
	}
	if rc := runFromscratch([]string{"-nope"}, io.Discard, io.Discard); rc != 2 {
		t.Errorf("bad flag: rc=%d", rc)
	}
	if rc := runFromscratch([]string{"-arch", "sparc", "-pantry", pantry, "-resolve-only", "root.org"}, io.Discard, io.Discard); rc != 1 {
		t.Errorf("unknown arch: rc=%d", rc)
	}
	if rc := runFromscratch([]string{"-arch", "arm64", "-pantry", filepath.Join(pantry, "nope"), "-resolve-only", "root.org"}, io.Discard, io.Discard); rc != 1 {
		t.Errorf("missing pantry: rc=%d", rc)
	}
	if rc := runFromscratch([]string{"-arch", "arm64", "-pantry", pantry, "root.org"}, io.Discard, io.Discard); rc != 2 {
		t.Errorf("missing tag/entrypoint: rc=%d", rc)
	}
	var out bytes.Buffer
	if rc := runFromscratch([]string{"-arch", "arm64", "-pantry", pantry, "-resolve-only", "root.org", "--"}, &out, io.Discard); rc != 0 {
		t.Errorf("resolve-only: rc=%d", rc)
	}
	if !strings.Contains(out.String(), "root.org:1.0") {
		t.Errorf("resolve-only out: %s", out.String())
	}
}

// buildELF crafts a minimal ET_DYN ELF whose .dynamic advertises the given
// DT_NEEDED sonames, so debug/elf can read them without a real toolchain.
func buildELF(needed []string) []byte {
	le := binary.LittleEndian
	var dynstr bytes.Buffer
	dynstr.WriteByte(0)
	off := map[string]uint64{}
	for _, n := range needed {
		off[n] = uint64(dynstr.Len())
		dynstr.WriteString(n)
		dynstr.WriteByte(0)
	}
	var dyn bytes.Buffer
	for _, n := range needed {
		var e [16]byte
		le.PutUint64(e[0:], 1) // DT_NEEDED
		le.PutUint64(e[8:], off[n])
		dyn.Write(e[:])
	}
	dyn.Write(make([]byte, 16)) // DT_NULL
	var shstr bytes.Buffer
	shstr.WriteByte(0)
	nameOff := func(s string) uint32 { o := uint32(shstr.Len()); shstr.WriteString(s); shstr.WriteByte(0); return o }
	nDynstr := nameOff(".dynstr")
	nDynamic := nameOff(".dynamic")
	nShstr := nameOff(".shstrtab")
	ehsize := uint64(64)
	dynstrOff := ehsize
	dynamicOff := dynstrOff + uint64(dynstr.Len())
	shstrOff := dynamicOff + uint64(dyn.Len())
	shoff := shstrOff + uint64(shstr.Len())
	buf := &bytes.Buffer{}
	hdr := make([]byte, 64)
	copy(hdr[0:], []byte{0x7f, 'E', 'L', 'F'})
	hdr[4], hdr[5], hdr[6] = 2, 1, 1
	le.PutUint16(hdr[16:], 3)
	le.PutUint16(hdr[18:], 62)
	le.PutUint32(hdr[20:], 1)
	le.PutUint64(hdr[40:], shoff)
	le.PutUint16(hdr[52:], 64)
	le.PutUint16(hdr[58:], 64)
	le.PutUint16(hdr[60:], 4)
	le.PutUint16(hdr[62:], 3)
	buf.Write(hdr)
	buf.Write(dynstr.Bytes())
	buf.Write(dyn.Bytes())
	buf.Write(shstr.Bytes())
	sh := func(name, typ uint32, offset, size uint64, link uint32, align, entsize uint64) []byte {
		b := make([]byte, 64)
		le.PutUint32(b[0:], name)
		le.PutUint32(b[4:], typ)
		le.PutUint64(b[24:], offset)
		le.PutUint64(b[32:], size)
		le.PutUint32(b[40:], link)
		le.PutUint64(b[48:], align)
		le.PutUint64(b[56:], entsize)
		return b
	}
	buf.Write(sh(0, 0, 0, 0, 0, 0, 0))
	buf.Write(sh(nDynstr, 3, dynstrOff, uint64(dynstr.Len()), 0, 1, 0))
	buf.Write(sh(nDynamic, 6, dynamicOff, uint64(dyn.Len()), 1, 8, 16))
	buf.Write(sh(nShstr, 3, shstrOff, uint64(shstr.Len()), 0, 1, 0))
	return buf.Bytes()
}

func TestCompleteElfClosure(t *testing.T) {
	root := t.TempDir()
	pkgx := filepath.Join(root, "pkgx")
	// A binary that NEEDs libcrypt.so.1 (undeclared) + an unmapped soname.
	bindir := filepath.Join(pkgx, "perl.org", "v1", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindir, "perl"), buildELF([]string{"libcrypt.so.1", "libc.so.6", "libmystery.so.9"}), 0o755); err != nil {
		t.Fatal(err)
	}
	// The libxcrypt provider bottle ships lib/libcrypt.so.1.
	xcryptBottle := makeTarGz(t, []tarEntry{
		{name: "github.com/besser82/libxcrypt/v4.5.2/lib/", typ: tar.TypeDir},
		{name: "github.com/besser82/libxcrypt/v4.5.2/lib/libcrypt.so.1", typ: tar.TypeReg, body: "so"},
	})
	f := &fakeOCI{
		tags:    map[string][]string{"github.com/besser82/libxcrypt": {"4.5.2"}},
		bottles: map[string][]byte{"github.com/besser82/libxcrypt@4.5.2": xcryptBottle},
	}
	var errb bytes.Buffer
	b := &fsBuilder{oc: f, arch: fsArch{OARCH: "arm64", PARCH: "aarch64"},
		stdout: io.Discard, stderr: &errb, cache: map[string]fsTarball{}}
	if err := b.completeElfClosure(root, pkgx); err != nil {
		t.Fatal(err)
	}
	// libxcrypt must have been pulled (readelf-driven) and extracted.
	if _, err := os.Stat(filepath.Join(pkgx, "github.com/besser82/libxcrypt/v4.5.2/lib/libcrypt.so.1")); err != nil {
		t.Fatalf("libxcrypt not pulled: %v", err)
	}
	// The unmapped soname must be warned about (once).
	if !strings.Contains(errb.String(), "unresolved soname libmystery.so.9") {
		t.Errorf("expected unresolved-soname warning, got: %s", errb.String())
	}
	// libc.so.6 (glibc) must NOT warn.
	if strings.Contains(errb.String(), "libc.so.6") {
		t.Errorf("glibc soname should be silent: %s", errb.String())
	}
}

func TestCompleteElfClosureUnpublishedProvider(t *testing.T) {
	root := t.TempDir()
	pkgx := filepath.Join(root, "pkgx")
	bindir := filepath.Join(pkgx, "tool", "v1", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	// NEEDs libz.so.1 whose provider (zlib.net) is not published in the fake.
	if err := os.WriteFile(filepath.Join(bindir, "tool"), buildELF([]string{"libz.so.1"}), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeOCI{tagsErr: map[string]bool{"zlib.net": true}}
	var errb bytes.Buffer
	b := &fsBuilder{oc: f, arch: fsArch{OARCH: "arm64", PARCH: "aarch64"},
		stdout: io.Discard, stderr: &errb, cache: map[string]fsTarball{}}
	if err := b.completeElfClosure(root, pkgx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "not published for linux/arm64") {
		t.Errorf("expected unpublished-provider warning, got: %s", errb.String())
	}
}

func TestBuildErrorBranches(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "tool.org", "")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	glibcWithLoader := makeTarGz(t, []tarEntry{
		{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/ld-linux-aarch64.so.1", typ: tar.TypeReg, body: "L", mode: 0o755},
	})
	glibcNoLoader := makeTarGz(t, []tarEntry{
		{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/libc.so.6", typ: tar.TypeReg, body: "C"},
	})
	toolBottle := makeTarGz(t, []tarEntry{
		{name: "tool.org/v1.0/lib/libtool.so.1", typ: tar.TypeReg, body: "so"},
	})
	newFake := func(glibc []byte) *fakeOCI {
		return &fakeOCI{
			tags:    map[string][]string{"tool.org": {"1.0"}, "gnu.org/glibc": {"2.44"}},
			bottles: map[string][]byte{"tool.org@1.0": toolBottle, "gnu.org/glibc@2.44": glibc},
		}
	}
	arch := fsArch{OARCH: "arm64", PARCH: "aarch64", Loader: "ld-linux-aarch64.so.1", Interp: "/lib/ld-linux-aarch64.so.1"}

	// (a) docker build fails → rc 1.
	origBuild := dockerBuild
	dockerBuild = func(string, string, string, io.Writer, io.Writer) error { return fmt.Errorf("boom") }
	b := &fsBuilder{oc: newFake(glibcWithLoader), pantry: pantry, arch: arch, stdout: io.Discard, stderr: io.Discard, cache: map[string]fsTarball{}}
	if rc := b.build(fsRequest{rootProj: "tool.org", tag: "img", entrypoint: "/x"}); rc != 1 {
		t.Errorf("docker build error: rc=%d", rc)
	}
	dockerBuild = origBuild

	// (b) docker run (smoke) fails → rc 1.
	origRun := dockerRun
	dockerBuild = func(string, string, string, io.Writer, io.Writer) error { return nil }
	dockerRun = func(string, string, []string, io.Writer, io.Writer) error { return fmt.Errorf("smoke") }
	b = &fsBuilder{oc: newFake(glibcWithLoader), pantry: pantry, arch: arch, stdout: io.Discard, stderr: io.Discard, cache: map[string]fsTarball{}}
	if rc := b.build(fsRequest{rootProj: "tool.org", tag: "img", entrypoint: "/x", run: true}); rc != 1 {
		t.Errorf("smoke error: rc=%d", rc)
	}
	dockerBuild, dockerRun = origBuild, origRun

	// (c) glibc bottle without the loader → findLoaderImage empty → rc 1.
	b = &fsBuilder{oc: newFake(glibcNoLoader), pantry: pantry, arch: arch, stdout: io.Discard, stderr: io.Discard, cache: map[string]fsTarball{}}
	if rc := b.build(fsRequest{rootProj: "tool.org", tag: "img", entrypoint: "/x"}); rc != 1 {
		t.Errorf("missing loader: rc=%d", rc)
	}
}

func writeFsRecipe(t *testing.T, pantry, proj, extra string) {
	t.Helper()
	dir := filepath.Join(pantry, "projects", proj)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "distributable:\n  url: https://x/{{version}}.tgz\nversions:\n  - 1.0\n" + extra
	if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
}
