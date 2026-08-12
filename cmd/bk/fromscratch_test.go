package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
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
	// A symlink (non-regular) with the loader name must be skipped, so create one
	// under a different version dir to exercise the IsRegular() guard.
	gdir2 := filepath.Join(root, "pkgx", "gnu.org", "glibc", "v2.43", "lib", "glibc-2.43")
	if err := os.MkdirAll(gdir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(gdir2, "ld-linux-x86-64.so.2")); err != nil {
		t.Fatal(err)
	}
	img := findLoaderImage(root, "ld-linux-aarch64.so.1")
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
	// A loader that exists only as a symlink → skipped → "".
	if img2 := findLoaderImage(root, "ld-linux-x86-64.so.2"); img2 != "" {
		t.Errorf("symlink loader must be skipped, got %q", img2)
	}
	// Absent glibc dir entirely → "" (walk callback swallows the missing-dir error).
	if img3 := findLoaderImage(t.TempDir(), "ld-linux-aarch64.so.1"); img3 != "" {
		t.Errorf("expected empty for absent glibc dir, got %q", img3)
	}
}

func TestSymlinkLoaderErrors(t *testing.T) {
	// MkdirAll fails: a regular file sits where a parent dir must be created.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lib"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := symlinkLoader(root, "/lib/sub/ld.so", "/img"); err == nil {
		t.Error("expected MkdirAll ENOTDIR error")
	}
	// Symlink fails: the destination is a non-empty dir that os.Remove can't drop.
	root2 := t.TempDir()
	busy := filepath.Join(root2, "lib", "ld-linux-aarch64.so.1")
	if err := os.MkdirAll(busy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(busy, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := symlinkLoader(root2, "/lib/ld-linux-aarch64.so.1", "/img"); err == nil {
		t.Error("expected Symlink EEXIST error")
	}
}

func TestWriteDockerfileError(t *testing.T) {
	// WriteFile fails when the workdir is actually a regular file.
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDockerfile(f, "/a", "/x"); err == nil {
		t.Error("expected WriteFile error")
	}
}

func TestDiscoverLibDirsError(t *testing.T) {
	// WalkDir surfaces the error for a non-existent root.
	if _, err := discoverLibDirs(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected WalkDir error on missing root")
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
func buildELF(needed []string) []byte { return buildELFCore(needed, "", false) }

// buildELFCore builds the ELF with an optional DT_SONAME. When badLink is set,
// the .dynamic section's string-table link is bogus, so f.DynString returns an
// error — exercising scanNeeded's DynString-error branch.
func buildELFCore(needed []string, soname string, badLink bool) []byte {
	le := binary.LittleEndian
	var dynstr bytes.Buffer
	dynstr.WriteByte(0)
	off := map[string]uint64{}
	record := func(n string) {
		if _, ok := off[n]; ok || n == "" {
			return
		}
		off[n] = uint64(dynstr.Len())
		dynstr.WriteString(n)
		dynstr.WriteByte(0)
	}
	for _, n := range needed {
		record(n)
	}
	record(soname)
	var dyn bytes.Buffer
	if soname != "" {
		var e [16]byte
		le.PutUint64(e[0:], 14) // DT_SONAME
		le.PutUint64(e[8:], off[soname])
		dyn.Write(e[:])
	}
	for _, n := range needed {
		var e [16]byte
		le.PutUint64(e[0:], 1) // DT_NEEDED
		le.PutUint64(e[8:], off[n])
		dyn.Write(e[:])
	}
	dyn.Write(make([]byte, 16)) // DT_NULL
	dynLink := uint32(1)        // .dynamic → .dynstr (section index 1)
	if badLink {
		dynLink = 99 // out-of-range → f.DynString string-table lookup errors
	}
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
	buf.Write(sh(nDynamic, 6, dynamicOff, uint64(dyn.Len()), dynLink, 8, 16))
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

// --- extractTar error branches ----------------------------------------------

// failCloseWriter is an io.WriteCloser whose Close always fails.
type failCloseWriter struct{ w io.Writer }

func (f failCloseWriter) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f failCloseWriter) Close() error                { return fmt.Errorf("close boom") }

func TestExtractTarErrorBranches(t *testing.T) {
	// tr.Next error: gzip of bytes that are not a valid tar.
	t.Run("bad-tar-stream", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write(bytes.Repeat([]byte{0xff}, 200))
		gz.Close()
		if err := extractBottle(buf.Bytes(), ".tar.gz", t.TempDir()); err == nil {
			t.Error("expected tar.Next error")
		}
	})
	// A regular file sits where a directory component must be created → ENOTDIR.
	underFile := func(t *testing.T, entries []tarEntry) error {
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return extractBottle(makeTarGz(t, entries), ".tar.gz", dest)
	}
	t.Run("dir-mkdirall", func(t *testing.T) {
		if err := underFile(t, []tarEntry{{name: "f/d/", typ: tar.TypeDir}}); err == nil {
			t.Error("expected dir MkdirAll ENOTDIR")
		}
	})
	t.Run("reg-mkdirall", func(t *testing.T) {
		if err := underFile(t, []tarEntry{{name: "f/x", typ: tar.TypeReg, body: "y"}}); err == nil {
			t.Error("expected reg MkdirAll ENOTDIR")
		}
	})
	t.Run("symlink-mkdirall", func(t *testing.T) {
		if err := underFile(t, []tarEntry{{name: "f/l", typ: tar.TypeSymlink, linkname: "t"}}); err == nil {
			t.Error("expected symlink MkdirAll ENOTDIR")
		}
	})
	t.Run("hardlink-mkdirall", func(t *testing.T) {
		if err := underFile(t, []tarEntry{{name: "f/l", typ: tar.TypeLink, linkname: "t"}}); err == nil {
			t.Error("expected hardlink MkdirAll ENOTDIR")
		}
	})
	// A non-empty directory sits where a regular file / symlink must be written.
	overBusyDir := func(t *testing.T, entries []tarEntry) error {
		dest := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dest, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "d", "keep"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return extractBottle(makeTarGz(t, entries), ".tar.gz", dest)
	}
	t.Run("reg-openfile-isdir", func(t *testing.T) {
		if err := overBusyDir(t, []tarEntry{{name: "d", typ: tar.TypeReg, body: "y"}}); err == nil {
			t.Error("expected OpenFile EISDIR")
		}
	})
	t.Run("symlink-eexist", func(t *testing.T) {
		if err := overBusyDir(t, []tarEntry{{name: "d", typ: tar.TypeSymlink, linkname: "t"}}); err == nil {
			t.Error("expected Symlink EEXIST")
		}
	})
	// Hardlink whose source does not exist → os.Link error.
	t.Run("hardlink-missing-src", func(t *testing.T) {
		if err := extractBottle(makeTarGz(t, []tarEntry{{name: "x", typ: tar.TypeLink, linkname: "nope"}}), ".tar.gz", t.TempDir()); err == nil {
			t.Error("expected Link error")
		}
	})
	// ioCopy error (seamed).
	t.Run("iocopy-error", func(t *testing.T) {
		orig := ioCopy
		ioCopy = func(io.Writer, io.Reader) (int64, error) { return 0, fmt.Errorf("copy boom") }
		defer func() { ioCopy = orig }()
		if err := extractBottle(makeTarGz(t, []tarEntry{{name: "ok", typ: tar.TypeReg, body: "y"}}), ".tar.gz", t.TempDir()); err == nil {
			t.Error("expected ioCopy error")
		}
	})
	// Close error (seamed).
	t.Run("close-error", func(t *testing.T) {
		orig := openFileWrite
		openFileWrite = func(name string, perm os.FileMode) (io.WriteCloser, error) {
			f, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
			if err != nil {
				return nil, err
			}
			return failCloseWriter{f}, nil
		}
		defer func() { openFileWrite = orig }()
		if err := extractBottle(makeTarGz(t, []tarEntry{{name: "ok", typ: tar.TypeReg, body: "y"}}), ".tar.gz", t.TempDir()); err == nil {
			t.Error("expected Close error")
		}
	})
}

// --- scanNeeded branches ----------------------------------------------------

func TestScanNeededSonameAndBadDynamic(t *testing.T) {
	root := t.TempDir()
	// An ELF advertising a DT_SONAME → recorded present (covers the SONAME loop).
	if err := os.WriteFile(filepath.Join(root, "lib.elf"), buildELFCore(nil, "libfoo.so.1", false), 0o755); err != nil {
		t.Fatal(err)
	}
	// An ELF whose .dynamic has a bogus string-table link → both DynString calls
	// error (covers the DT_SONAME-error skip AND the DT_NEEDED-error return).
	if err := os.WriteFile(filepath.Join(root, "corrupt.bin"), buildELFCore([]string{"libx.so"}, "", true), 0o755); err != nil {
		t.Fatal(err)
	}
	needed, present, err := scanNeeded(root)
	if err != nil {
		t.Fatal(err)
	}
	if !present["libfoo.so.1"] {
		t.Errorf("DT_SONAME not recorded present: %v", present)
	}
	if len(needed) != 0 {
		t.Errorf("corrupt-dynamic ELF must contribute no NEEDED: %v", needed)
	}
}

func TestDiscoverLibDirsRootLevel(t *testing.T) {
	// A shared object directly at the root → its dir equals root → rel becomes "/".
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "libtop.so.1"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs, err := discoverLibDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "/" {
		t.Fatalf("root-level .so → dirs = %v, want [/]", dirs)
	}
}

func TestExtractBottleXz(t *testing.T) {
	// A valid xz-compressed tar exercises the .tar.xz decode path.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	_ = tw.WriteHeader(&tar.Header{Name: "p/v1/x", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644})
	_, _ = tw.Write([]byte("hi"))
	tw.Close()
	var comp bytes.Buffer
	xw, err := xz.NewWriter(&comp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	xw.Close()
	dest := t.TempDir()
	if err := extractBottle(comp.Bytes(), ".tar.xz", dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "p/v1/x")); err != nil || string(b) != "hi" {
		t.Fatalf("xz extract: %v %q", err, b)
	}
}

// TestHelperProcess is the child process used to drive the real docker exec
// bodies without docker installed: it exits 0, or 1 when GO_HELPER_FAIL=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	if os.Getenv("GO_HELPER_FAIL") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeExec(fail bool) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		c := exec.Command(os.Args[0], cs...)
		c.Env = []string{"GO_WANT_HELPER_PROCESS=1"}
		if fail {
			c.Env = append(c.Env, "GO_HELPER_FAIL=1")
		}
		return c
	}
}

func TestDockerBuildAndRunReal(t *testing.T) {
	orig := execCommand
	defer func() { execCommand = orig }()

	execCommand = fakeExec(false)
	if err := dockerBuild("/w", "arm64", "img", io.Discard, io.Discard); err != nil {
		t.Errorf("dockerBuild success: %v", err)
	}
	if err := dockerRun("arm64", "img", []string{"--version"}, io.Discard, io.Discard); err != nil {
		t.Errorf("dockerRun success: %v", err)
	}
	execCommand = fakeExec(true)
	if err := dockerBuild("/w", "arm64", "img", io.Discard, io.Discard); err == nil {
		t.Error("dockerBuild expected failure")
	}
	if err := dockerRun("arm64", "img", nil, io.Discard, io.Discard); err == nil {
		t.Error("dockerRun expected failure")
	}
}

func TestScanNeededWalkError(t *testing.T) {
	if _, _, err := scanNeeded(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected WalkDir error on missing root")
	}
}

// --- recipeDeps / declaredClosure branches ----------------------------------

func TestRecipeDepsMalformed(t *testing.T) {
	pantry := t.TempDir()
	dir := filepath.Join(pantry, "projects", "bad.org")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Readable but not valid → pantry.Parse errors → ok=false.
	if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte("::: not yaml :::\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := recipeDeps(pantry, "bad.org", "aarch64"); ok {
		t.Error("malformed recipe should return ok=false")
	}
}

func TestDeclaredClosureLeafAndDiamond(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "root.org", "dependencies:\n  a.org: '*'\n  b.org: '*'\n  missing.org: '*'\n")
	writeFsRecipe(t, pantry, "a.org", "dependencies:\n  c.org: '*'\n")
	writeFsRecipe(t, pantry, "b.org", "dependencies:\n  c.org: '*'\n")
	writeFsRecipe(t, pantry, "c.org", "")
	var errb bytes.Buffer
	b := &fsBuilder{pantry: pantry, arch: fsArch{PARCH: "aarch64"}, stderr: &errb}
	order := b.declaredClosure("root.org")
	// c.org appears exactly once (diamond → seen-set), glibc first.
	n := 0
	for _, p := range order {
		if p == "c.org" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("c.org should appear once: %v", order)
	}
	if !strings.Contains(errb.String(), "no recipe for missing.org (leaf)") {
		t.Errorf("expected leaf warning, got: %s", errb.String())
	}
}

// --- resolveBottle branches -------------------------------------------------

func TestResolveBottleBranches(t *testing.T) {
	// Newest tag lacks the arch bottle → falls back to the older one.
	f := &fakeOCI{
		tags:    map[string][]string{"p.org": {"2.0", "1.0"}},
		bottles: map[string][]byte{"p.org@1.0": makeTarGz(t, []tarEntry{{name: "p.org/v1.0/", typ: tar.TypeDir}})},
	}
	b := &fsBuilder{oc: f, arch: fsArch{OARCH: "arm64"}, cache: map[string]fsTarball{}}
	got, ok := b.resolveBottle("p.org")
	if !ok || got.ver != "1.0" {
		t.Fatalf("expected fallback to 1.0, got %q ok=%v", got.ver, ok)
	}
	// Second call → cache hit (positive).
	if g2, ok2 := b.resolveBottle("p.org"); !ok2 || g2.ver != "1.0" {
		t.Fatalf("cache hit: %q ok=%v", g2.ver, ok2)
	}
	// A project whose every version lacks the arch → negative cache, then hit.
	f2 := &fakeOCI{tags: map[string][]string{"q.org": {"1.0"}}}
	b2 := &fsBuilder{oc: f2, arch: fsArch{OARCH: "arm64"}, cache: map[string]fsTarball{}}
	if _, ok := b2.resolveBottle("q.org"); ok {
		t.Fatal("expected unpublished (no bottle)")
	}
	if _, ok := b2.resolveBottle("q.org"); ok { // negative cache hit
		t.Fatal("expected negative cache hit")
	}
}

// --- completeElfClosure branches --------------------------------------------

func TestCompleteElfClosureMoreBranches(t *testing.T) {
	mkRootNeeding := func(t *testing.T, soname string) (root, pkgx string) {
		root = t.TempDir()
		pkgx = filepath.Join(root, "pkgx")
		bindir := filepath.Join(pkgx, "tool", "v1", "bin")
		if err := os.MkdirAll(bindir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bindir, "t"), buildELF([]string{soname}), 0o755); err != nil {
			t.Fatal(err)
		}
		return root, pkgx
	}

	t.Run("scan-error", func(t *testing.T) {
		b := &fsBuilder{arch: fsArch{OARCH: "arm64"}, stderr: io.Discard, cache: map[string]fsTarball{}}
		if err := b.completeElfClosure(filepath.Join(t.TempDir(), "missing"), "x"); err == nil {
			t.Error("expected scanNeeded error")
		}
	})

	t.Run("negative-cached-provider", func(t *testing.T) {
		root, pkgx := mkRootNeeding(t, "libz.so.1")
		b := &fsBuilder{arch: fsArch{OARCH: "arm64"}, stderr: io.Discard,
			cache: map[string]fsTarball{"zlib.net": {}}} // negative-cached
		if err := b.completeElfClosure(root, pkgx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("prefix-already-present", func(t *testing.T) {
		root, pkgx := mkRootNeeding(t, "libz.so.1")
		// Positive cache + pre-created prefix → extract is skipped.
		if err := os.MkdirAll(filepath.Join(pkgx, "zlib.net", "v1.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		b := &fsBuilder{arch: fsArch{OARCH: "arm64"}, stderr: io.Discard,
			cache: map[string]fsTarball{"zlib.net": {ver: "1.0", data: []byte("unused"), ext: ".tar.gz"}}}
		if err := b.completeElfClosure(root, pkgx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("provider-extract-error", func(t *testing.T) {
		root, pkgx := mkRootNeeding(t, "libz.so.1")
		f := &fakeOCI{
			tags:    map[string][]string{"zlib.net": {"1.0"}},
			bottles: map[string][]byte{"zlib.net@1.0": []byte("not a gzip")},
		}
		b := &fsBuilder{oc: f, arch: fsArch{OARCH: "arm64"}, stderr: io.Discard, cache: map[string]fsTarball{}}
		if err := b.completeElfClosure(root, pkgx); err == nil {
			t.Error("expected provider extract error")
		}
	})
}

// --- build error branches (via seams) ---------------------------------------

func TestBuildSeamErrorBranches(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "tool.org", "")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	glibc := makeTarGz(t, []tarEntry{
		{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/ld-linux-aarch64.so.1", typ: tar.TypeReg, body: "L", mode: 0o755},
	})
	tool := makeTarGz(t, []tarEntry{{name: "tool.org/v1.0/lib/libtool.so.1", typ: tar.TypeReg, body: "so"}})
	arch := fsArch{OARCH: "arm64", PARCH: "aarch64", Loader: "ld-linux-aarch64.so.1", Interp: "/lib/ld-linux-aarch64.so.1"}
	newB := func(glibcBottle, toolBottle []byte) *fsBuilder {
		f := &fakeOCI{
			tags:    map[string][]string{"tool.org": {"1.0"}, "gnu.org/glibc": {"2.44"}},
			bottles: map[string][]byte{"tool.org@1.0": toolBottle, "gnu.org/glibc@2.44": glibcBottle},
		}
		return &fsBuilder{oc: f, pantry: pantry, arch: arch, stdout: io.Discard, stderr: io.Discard, cache: map[string]fsTarball{}}
	}
	req := fsRequest{rootProj: "tool.org", tag: "img", entrypoint: "/pkgx/tool.org/{V}/bin/x"}

	restore := func(fns ...func()) func() {
		return func() {
			for _, f := range fns {
				f()
			}
		}
	}

	// fsMkdirTemp error.
	t.Run("mkdirtemp", func(t *testing.T) {
		orig := fsMkdirTemp
		defer restore(func() { fsMkdirTemp = orig })()
		fsMkdirTemp = func(string, string) (string, error) { return "", fmt.Errorf("temp boom") }
		if rc := newB(glibc, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// MkdirAll(pkgx) error: workdir is a regular file.
	t.Run("mkdirall-pkgx", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		orig := fsMkdirTemp
		defer restore(func() { fsMkdirTemp = orig })()
		fsMkdirTemp = func(string, string) (string, error) { return f, nil }
		if rc := newB(glibc, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// extract error: the root bottle is not a valid tarball.
	t.Run("extract", func(t *testing.T) {
		if rc := newB(glibc, []byte("not gzip")).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// completeElfClosure error: a provider bottle is corrupt.
	t.Run("complete-closure", func(t *testing.T) {
		toolNeedsZ := makeTarGz(t, []tarEntry{{name: "tool.org/v1.0/bin/t", typ: tar.TypeReg, body: string(buildELF([]string{"libz.so.1"})), mode: 0o755}})
		f := &fakeOCI{
			tags: map[string][]string{"tool.org": {"1.0"}, "gnu.org/glibc": {"2.44"}, "zlib.net": {"1.0"}},
			bottles: map[string][]byte{
				"tool.org@1.0": toolNeedsZ, "gnu.org/glibc@2.44": glibc, "zlib.net@1.0": []byte("bad"),
			},
		}
		b := &fsBuilder{oc: f, pantry: pantry, arch: arch, stdout: io.Discard, stderr: io.Discard, cache: map[string]fsTarball{}}
		if rc := b.build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// loader missing in the glibc bottle.
	t.Run("no-loader", func(t *testing.T) {
		noLoader := makeTarGz(t, []tarEntry{{name: "gnu.org/glibc/v2.44/lib/glibc-2.44/libc.so.6", typ: tar.TypeReg, body: "C"}})
		if rc := newB(noLoader, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// symlinkLoader error (seamed).
	t.Run("symlink", func(t *testing.T) {
		orig := symlinkLoader
		defer restore(func() { symlinkLoader = orig })()
		symlinkLoader = func(string, string, string) error { return fmt.Errorf("link boom") }
		if rc := newB(glibc, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// discoverLibDirs error (seamed).
	t.Run("discover-error", func(t *testing.T) {
		orig := discoverLibDirs
		defer restore(func() { discoverLibDirs = orig })()
		discoverLibDirs = func(string) ([]string, error) { return nil, fmt.Errorf("walk boom") }
		if rc := newB(glibc, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// discoverLibDirs empty → error (seamed).
	t.Run("discover-empty", func(t *testing.T) {
		orig := discoverLibDirs
		defer restore(func() { discoverLibDirs = orig })()
		discoverLibDirs = func(string) ([]string, error) { return nil, nil }
		if rc := newB(glibc, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// writeDockerfile error (seamed).
	t.Run("dockerfile", func(t *testing.T) {
		orig := writeDockerfile
		defer restore(func() { writeDockerfile = orig })()
		writeDockerfile = func(string, string, string) error { return fmt.Errorf("df boom") }
		origB := dockerBuild
		defer restore(func() { dockerBuild = origB })()
		dockerBuild = func(string, string, string, io.Writer, io.Writer) error { return nil }
		if rc := newB(glibc, tool).build(req); rc != 1 {
			t.Errorf("rc=%d", rc)
		}
	})

	// keep=true: workdir is not removed (exercises the keep branch); docker seamed OK.
	t.Run("keep", func(t *testing.T) {
		origB, origR := dockerBuild, dockerRun
		defer restore(func() { dockerBuild, dockerRun = origB, origR })()
		dockerBuild = func(string, string, string, io.Writer, io.Writer) error { return nil }
		if rc := newB(glibc, tool).build(fsRequest{rootProj: "tool.org", tag: "img", entrypoint: "/x", keep: true}); rc != 0 {
			t.Errorf("rc=%d", rc)
		}
	})
}

// --- runFromscratch / dispatch ----------------------------------------------

func TestRunFromscratchNewOCIError(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "root.org", "")
	orig := newOCIClient
	newOCIClient = func(string) (ociPuller, error) { return nil, fmt.Errorf("oci boom") }
	defer func() { newOCIClient = orig }()
	if rc := runFromscratch([]string{"-arch", "arm64", "-pantry", pantry, "-resolve-only", "root.org"}, io.Discard, io.Discard); rc != 1 {
		t.Errorf("expected rc 1 on OCI client error, got %d", rc)
	}
}

func TestNewOCIClientDefault(t *testing.T) {
	// The default seam body just constructs the client (parses the base, no I/O).
	if _, err := newOCIClient("oci://ghcr.io/go-pkgx/packages"); err != nil {
		t.Fatalf("default newOCIClient: %v", err)
	}
}

func TestRunDispatchesFromscratch(t *testing.T) {
	pantry := t.TempDir()
	writeFsRecipe(t, pantry, "root.org", "")
	writeFsRecipe(t, pantry, "gnu.org/glibc", "")
	f := &fakeOCI{
		tags: map[string][]string{"root.org": {"1.0"}, "gnu.org/glibc": {"2.44"}},
		bottles: map[string][]byte{
			"root.org@1.0":       makeTarGz(t, []tarEntry{{name: "root.org/v1.0/", typ: tar.TypeDir}}),
			"gnu.org/glibc@2.44": makeTarGz(t, []tarEntry{{name: "gnu.org/glibc/v2.44/", typ: tar.TypeDir}}),
		},
	}
	orig := newOCIClient
	newOCIClient = func(string) (ociPuller, error) { return f, nil }
	defer func() { newOCIClient = orig }()
	var out bytes.Buffer
	if rc := run([]string{"fromscratch", "-arch", "arm64", "-pantry", pantry, "-resolve-only", "root.org"}, &out, io.Discard); rc != 0 {
		t.Fatalf("run dispatch rc=%d", rc)
	}
	if !strings.Contains(out.String(), "root.org:1.0") {
		t.Errorf("run dispatch output: %s", out.String())
	}
}
