package fetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// restoreSeams resets every injectable seam after the test.
func restoreSeams(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		httpGet = http.Get
		execCommand = exec.Command
		osMkdirAll = os.MkdirAll
		osSymlink = os.Symlink
		osOpenFile = os.OpenFile
		ioCopy = io.Copy
		zipOpen = defaultZipOpen
	})
}

// serve starts a test server that answers every request with body.
func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

type tarEntry struct {
	name string
	typ  byte
	mode int64
	body string
	link string
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func gzWrap(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func xzWrap(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	if _, err := xw.Write(data); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := xw.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name    string
	mode    fs.FileMode // 0 means "leave unset" (no Unix mode bits)
	body    string
	symlink bool
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.symlink {
			hdr.SetMode(e.mode.Perm() | fs.ModeSymlink)
		} else if e.mode != 0 {
			if strings.HasSuffix(e.name, "/") {
				hdr.SetMode(e.mode.Perm() | fs.ModeDir)
			} else {
				hdr.SetMode(e.mode.Perm())
			}
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip write %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// bz2TarHex is a bzip2-compressed tar holding "bz/" (0755) and
// "bz/hello.txt" (0644, "hello bz2\n"). compress/bzip2 has no writer, so
// the fixture is pre-generated (python3 tarfile w:bz2) and embedded.
const bz2TarHex = "425a6839314159265359dad7470f00008afb80ca9001004001ff80040272449e50080820007412921a1a0d01881a3209293268d00001a03eea8baa4e48083d2421ce897b6014dcc1e4c210c01b228923359ba841628107c29107546d752caad6aae5b610b2f11c3e6c8947113d08922733e918cd321103f177245385090dad7470f0"

func bz2Tar(t *testing.T) []byte {
	t.Helper()
	data, err := hex.DecodeString(bz2TarHex)
	if err != nil {
		t.Fatalf("decode bz2 fixture: %v", err)
	}
	return data
}

// sampleTar is a representative source tree: a dir, a regular file, an
// executable, a symlink, a fifo (skipped) and a "./" entry (skipped).
func sampleTar(t *testing.T) []byte {
	return buildTar(t, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "src/", typ: tar.TypeDir, mode: 0o755},
		{name: "src/main.c", typ: tar.TypeReg, mode: 0o644, body: "int main(){}\n"},
		{name: "configure", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
		{name: "src/link.c", typ: tar.TypeSymlink, mode: 0o777, link: "main.c"},
		{name: "src/fifo", typ: tar.TypeFifo, mode: 0o644},
	})
}

func checkSampleTree(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "src", "main.c"))
	if err != nil || string(got) != "int main(){}\n" {
		t.Fatalf("main.c = %q, %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "src", "main.c"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("main.c mode = %v, %v; want 0644", fi.Mode(), err)
	}
	fi, err = os.Stat(filepath.Join(dir, "configure"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("configure mode = %v, %v; want 0755", fi.Mode(), err)
	}
	fi, err = os.Stat(filepath.Join(dir, "src"))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("src dir = %v, %v; want dir 0755", fi.Mode(), err)
	}
	link, err := os.Readlink(filepath.Join(dir, "src", "link.c"))
	if err != nil || link != "main.c" {
		t.Fatalf("link.c -> %q, %v; want main.c", link, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "src", "fifo")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fifo should be skipped, Lstat err = %v", err)
	}
}

func TestFetchTarGz(t *testing.T) {
	s := serve(t, gzWrap(t, sampleTar(t)))
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg.tar.gz", dir, 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	checkSampleTree(t, dir)
}

func TestFetchTgzWithQueryString(t *testing.T) {
	s := serve(t, gzWrap(t, sampleTar(t)))
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg.tgz?token=abc#frag", dir, 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	checkSampleTree(t, dir)
}

func TestFetchTarXz(t *testing.T) {
	s := serve(t, xzWrap(t, sampleTar(t)))
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg.tar.xz", dir, 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	checkSampleTree(t, dir)
}

func TestFetchTarPlain(t *testing.T) {
	s := serve(t, sampleTar(t))
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg.tar", dir, 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	checkSampleTree(t, dir)
}

func TestFetchTarBz2(t *testing.T) {
	for _, ext := range []string{".tar.bz2", ".tbz2"} {
		s := serve(t, bz2Tar(t))
		dir := t.TempDir()
		if err := Fetch(s.URL+"/pkg"+ext, dir, 0); err != nil {
			t.Fatalf("Fetch(%s): %v", ext, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "bz", "hello.txt"))
		if err != nil || string(got) != "hello bz2\n" {
			t.Fatalf("hello.txt = %q, %v", got, err)
		}
	}
}

func TestFetchStripComponents(t *testing.T) {
	data := gzWrap(t, buildTar(t, []tarEntry{
		{name: "pkg-1.0/", typ: tar.TypeDir, mode: 0o755},
		{name: "pkg-1.0/src/", typ: tar.TypeDir, mode: 0o755},
		{name: "pkg-1.0/src/main.c", typ: tar.TypeReg, mode: 0o644, body: "x\n"},
	}))
	s := serve(t, data)
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg-1.0.tar.gz", dir, 1); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "src", "main.c")); err != nil || string(got) != "x\n" {
		t.Fatalf("src/main.c = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg-1.0")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pkg-1.0 should be stripped, err = %v", err)
	}
}

func TestFetchZip(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "pkg/", mode: 0o755},
		{name: "pkg/nomode/"}, // no Unix mode bits: permOr fallback
		{name: "pkg/readme.txt", mode: 0o644, body: "zip body\n"},
		{name: "pkg/run", mode: 0o755, body: "#!/bin/sh\n"},
		{name: "pkg/alias.txt", mode: 0o777, symlink: true, body: "readme.txt"},
	})
	s := serve(t, data)
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg.zip", dir, 1); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "readme.txt")); err != nil || string(got) != "zip body\n" {
		t.Fatalf("readme.txt = %q, %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "run"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run mode = %v, %v; want 0755", fi.Mode(), err)
	}
	// Entries without Unix mode bits get MS-DOS-derived perms from
	// archive/zip; the perm-0 fallback itself is covered by
	// TestFetchTarZeroModeFallbacks.
	fi, err = os.Stat(filepath.Join(dir, "nomode"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("nomode = %v, %v; want dir", fi, err)
	}
	link, err := os.Readlink(filepath.Join(dir, "alias.txt"))
	if err != nil || link != "readme.txt" {
		t.Fatalf("alias.txt -> %q, %v; want readme.txt", link, err)
	}
}

func TestFetchUnknownExtension(t *testing.T) {
	err := Fetch("http://example.invalid/pkg.rar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "unknown archive extension") {
		t.Fatalf("err = %v; want unknown archive extension", err)
	}
}

func TestFetchHTTPGetError(t *testing.T) {
	err := Fetch("http://127.0.0.1:1/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "fetch: GET") {
		t.Fatalf("err = %v; want GET error", err)
	}
}

func TestFetchHTTPStatusNotOK(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(http.NotFound))
	t.Cleanup(s.Close)
	err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v; want 404", err)
	}
}

func TestFetchDestDirCreateError(t *testing.T) {
	s := serve(t, sampleTar(t))
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Fetch(s.URL+"/pkg.tar", filepath.Join(blocker, "dest"), 0)
	if err == nil || !strings.Contains(err.Error(), "fetch: create") {
		t.Fatalf("err = %v; want create error", err)
	}
}

func TestFetchBadGzip(t *testing.T) {
	s := serve(t, []byte("not gzip"))
	err := Fetch(s.URL+"/pkg.tar.gz", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "read gzip") {
		t.Fatalf("err = %v; want read gzip error", err)
	}
}

func TestFetchBadXz(t *testing.T) {
	s := serve(t, []byte("not xz at all"))
	err := Fetch(s.URL+"/pkg.tar.xz", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "read xz") {
		t.Fatalf("err = %v; want read xz error", err)
	}
}

func TestFetchBadZip(t *testing.T) {
	s := serve(t, []byte("not a zip"))
	err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "read zip") {
		t.Fatalf("err = %v; want read zip error", err)
	}
}

func TestFetchMalformedTar(t *testing.T) {
	s := serve(t, bytes.Repeat([]byte{'x'}, 1024))
	err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "read tar") {
		t.Fatalf("err = %v; want read tar error", err)
	}
}

func TestFetchZipBodyReadError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(s.Close)
	err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Fatalf("err = %v; want read body error", err)
	}
}

func TestFetchTarPathTraversal(t *testing.T) {
	data := buildTar(t, []tarEntry{
		{name: "../evil.txt", typ: tar.TypeReg, mode: 0o644, body: "evil"},
	})
	s := serve(t, data)
	dir := t.TempDir()
	err := Fetch(s.URL+"/pkg.tar", dir, 0)
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("err = %v; want path traversal", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("evil.txt escaped destDir, err = %v", err)
	}
}

func TestFetchTarAbsolutePath(t *testing.T) {
	data := buildTar(t, []tarEntry{
		{name: "/abs.txt", typ: tar.TypeReg, mode: 0o644, body: "evil"},
	})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("err = %v; want absolute path", err)
	}
}

func TestFetchZipPathTraversal(t *testing.T) {
	data := buildZip(t, []zipEntry{
		{name: "../evil.txt", mode: 0o644, body: "evil"},
	})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("err = %v; want path traversal", err)
	}
}

// failMkdirOn returns an osMkdirAll that fails for paths containing marker.
func failMkdirOn(marker string) func(string, fs.FileMode) error {
	return func(p string, m fs.FileMode) error {
		if strings.Contains(p, marker) {
			return errors.New("mkdir boom")
		}
		return os.MkdirAll(p, m)
	}
}

func TestFetchTarMkdirErrors(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"dir entry", []tarEntry{{name: "boomdir/", typ: tar.TypeDir, mode: 0o755}}},
		{"symlink parent", []tarEntry{{name: "boomdir/l", typ: tar.TypeSymlink, mode: 0o777, link: "x"}}},
		{"file parent", []tarEntry{{name: "boomdir/f", typ: tar.TypeReg, mode: 0o644, body: "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreSeams(t)
			osMkdirAll = failMkdirOn("boomdir")
			s := serve(t, buildTar(t, tc.entries))
			err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
			if err == nil || !strings.Contains(err.Error(), "mkdir boom") {
				t.Fatalf("err = %v; want mkdir boom", err)
			}
		})
	}
}

func TestFetchZipMkdirErrors(t *testing.T) {
	cases := []struct {
		name    string
		entries []zipEntry
	}{
		{"dir entry", []zipEntry{{name: "boomdir/", mode: 0o755}}},
		{"symlink parent", []zipEntry{{name: "boomdir/l", mode: 0o777, symlink: true, body: "x"}}},
		{"file parent", []zipEntry{{name: "boomdir/f", mode: 0o644, body: "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreSeams(t)
			osMkdirAll = failMkdirOn("boomdir")
			s := serve(t, buildZip(t, tc.entries))
			err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
			if err == nil || !strings.Contains(err.Error(), "mkdir boom") {
				t.Fatalf("err = %v; want mkdir boom", err)
			}
		})
	}
}

func TestFetchTarSymlinkError(t *testing.T) {
	restoreSeams(t)
	osSymlink = func(_, _ string) error { return errors.New("symlink boom") }
	data := buildTar(t, []tarEntry{{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "x"}})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "symlink boom") {
		t.Fatalf("err = %v; want symlink boom", err)
	}
}

func TestFetchZipSymlinkError(t *testing.T) {
	restoreSeams(t)
	osSymlink = func(_, _ string) error { return errors.New("symlink boom") }
	data := buildZip(t, []zipEntry{{name: "l", mode: 0o777, symlink: true, body: "x"}})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "symlink boom") {
		t.Fatalf("err = %v; want symlink boom", err)
	}
}

func TestFetchTarOpenFileError(t *testing.T) {
	restoreSeams(t)
	osOpenFile = func(string, int, fs.FileMode) (*os.File, error) { return nil, errors.New("open boom") }
	data := buildTar(t, []tarEntry{{name: "f", typ: tar.TypeReg, mode: 0o644, body: "x"}})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "open boom") {
		t.Fatalf("err = %v; want open boom", err)
	}
}

func TestFetchZipOpenFileError(t *testing.T) {
	restoreSeams(t)
	osOpenFile = func(string, int, fs.FileMode) (*os.File, error) { return nil, errors.New("open boom") }
	data := buildZip(t, []zipEntry{{name: "f", mode: 0o644, body: "x"}})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "open boom") {
		t.Fatalf("err = %v; want open boom", err)
	}
}

func TestFetchTarCopyError(t *testing.T) {
	restoreSeams(t)
	ioCopy = func(io.Writer, io.Reader) (int64, error) { return 0, errors.New("copy boom") }
	data := buildTar(t, []tarEntry{{name: "f", typ: tar.TypeReg, mode: 0o644, body: "x"}})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.tar", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "copy boom") {
		t.Fatalf("err = %v; want copy boom", err)
	}
}

func TestFetchZipSymlinkCopyError(t *testing.T) {
	restoreSeams(t)
	ioCopy = func(io.Writer, io.Reader) (int64, error) { return 0, errors.New("copy boom") }
	data := buildZip(t, []zipEntry{{name: "l", mode: 0o777, symlink: true, body: "x"}})
	s := serve(t, data)
	err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "copy boom") {
		t.Fatalf("err = %v; want copy boom", err)
	}
}

func TestFetchZipEntryOpenErrors(t *testing.T) {
	cases := []struct {
		name    string
		entries []zipEntry
	}{
		{"symlink entry", []zipEntry{{name: "l", mode: 0o777, symlink: true, body: "x"}}},
		{"file entry", []zipEntry{{name: "f", mode: 0o644, body: "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreSeams(t)
			zipOpen = func(*zip.File) (io.ReadCloser, error) { return nil, errors.New("zip open boom") }
			s := serve(t, buildZip(t, tc.entries))
			err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0)
			if err == nil || !strings.Contains(err.Error(), "zip open boom") {
				t.Fatalf("err = %v; want zip open boom", err)
			}
		})
	}
}

func TestFetchTarZeroModeFallbacks(t *testing.T) {
	data := buildTar(t, []tarEntry{
		{name: "d/", typ: tar.TypeDir, mode: 0},
		{name: "d/f", typ: tar.TypeReg, mode: 0, body: "x"},
	})
	s := serve(t, data)
	dir := t.TempDir()
	if err := Fetch(s.URL+"/pkg.tar", dir, 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "d"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("d mode = %v, %v; want 0755 fallback", fi.Mode(), err)
	}
	fi, err = os.Stat(filepath.Join(dir, "d", "f"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("d/f mode = %v, %v; want 0644 fallback", fi.Mode(), err)
	}
}

func TestSafeTargetSkipsFullyStripped(t *testing.T) {
	if _, ok, err := safeTarget("/dest", "pkg-1.0", 1); err != nil || ok {
		t.Fatalf("safeTarget = ok %v, err %v; want skip", ok, err)
	}
	if _, ok, err := safeTarget("/dest", "./", 0); err != nil || ok {
		t.Fatalf("safeTarget(./) = ok %v, err %v; want skip", ok, err)
	}
}

func TestFetchGit(t *testing.T) {
	restoreSeams(t)
	var gotArgs []string
	execCommand = func(name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{name}, args...)
		return exec.Command("true")
	}
	if err := FetchGit("https://example.com/r.git", "v1.2.3", "/tmp/dest"); err != nil {
		t.Fatalf("FetchGit: %v", err)
	}
	want := []string{"git", "clone", "--depth", "1", "--branch", "v1.2.3", "https://example.com/r.git", "/tmp/dest"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v; want %v", gotArgs, want)
	}
}

func TestFetchGitError(t *testing.T) {
	restoreSeams(t)
	execCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "echo clone failed >&2; exit 3")
	}
	err := FetchGit("https://example.com/r.git", "main", "/tmp/dest")
	if err == nil || !strings.Contains(err.Error(), "clone failed") {
		t.Fatalf("err = %v; want clone failed output", err)
	}
}
