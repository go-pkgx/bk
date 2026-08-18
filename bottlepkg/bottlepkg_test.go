package bottlepkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/go-pkgx/bottle"
	"github.com/klauspost/compress/zstd"
)

var errBoom = errors.New("boom")

// errWriter fails every Write, so the eager gzip header write fails.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errBoom }

// fakeWC is an io.WriteCloser with injectable Write/Close failures.
type fakeWC struct {
	writeErr error
	closeErr error
	buf      bytes.Buffer
}

func (f *fakeWC) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}

func (f *fakeWC) Close() error { return f.closeErr }

const (
	project = "openssl.org"
	version = "1.1.1w"
	prefix  = project + "/v" + version + "/"
)

// makeTree builds an install tree with a hidden file, an executable, a
// nested subdirectory, and a symlink.
func makeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"bin", "lib/nested"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(dir, "bin", "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "nested", "data.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../bin/tool", filepath.Join(dir, "lib", "link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

type entry struct {
	hdr  tar.Header
	body []byte
}

// readBottle decompresses and parses a bottle, preserving entry order.
// readBottle decodes a bottle with whatever codec Codec currently names, so the
// structural assertions below follow the DEFAULT rather than pinning gzip — a
// default that silently stopped producing a readable tar would go unnoticed
// otherwise.
func readBottle(t *testing.T, r io.Reader) ([]string, map[string]entry) {
	t.Helper()
	var dec io.Reader
	switch Codec {
	case bottle.ExtTarZst:
		z, err := zstd.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(z.Close)
		dec = z
	default:
		gz, err := gzip.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		dec = gz
	}
	tr := tar.NewReader(dec)
	var order []string
	entries := map[string]entry{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, hdr.Name)
		entries[hdr.Name] = entry{hdr: *hdr, body: body}
	}
	return order, entries
}

func TestBottleRoundTrip(t *testing.T) {
	dir := makeTree(t)
	var buf bytes.Buffer
	if err := Bottle(dir, project, version, &buf); err != nil {
		t.Fatal(err)
	}
	order, entries := readBottle(t, &buf)

	want := []string{
		prefix + ".hidden",
		prefix + "bin/",
		prefix + "bin/tool",
		prefix + "lib/",
		prefix + "lib/link",
		prefix + "lib/nested/",
		prefix + "lib/nested/data.txt",
	}
	if len(order) != len(want) {
		t.Fatalf("got %d entries %v, want %d", len(order), order, len(want))
	}
	for i, name := range want {
		if order[i] != name {
			t.Fatalf("entry %d = %q, want %q (order %v)", i, order[i], name, order)
		}
	}
	if !sort.StringsAreSorted(order) {
		t.Fatalf("entries not sorted: %v", order)
	}

	tool := entries[prefix+"bin/tool"]
	if tool.hdr.Typeflag != tar.TypeReg {
		t.Fatalf("bin/tool typeflag = %v, want TypeReg", tool.hdr.Typeflag)
	}
	if m := tool.hdr.FileInfo().Mode().Perm(); m != 0o755 {
		t.Fatalf("bin/tool mode = %o, want 0755", m)
	}
	if string(tool.body) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("bin/tool body = %q", tool.body)
	}

	link := entries[prefix+"lib/link"]
	if link.hdr.Typeflag != tar.TypeSymlink {
		t.Fatalf("lib/link typeflag = %v, want TypeSymlink", link.hdr.Typeflag)
	}
	if link.hdr.Linkname != "../bin/tool" {
		t.Fatalf("lib/link target = %q, want ../bin/tool", link.hdr.Linkname)
	}

	if d := entries[prefix+"bin/"]; d.hdr.Typeflag != tar.TypeDir {
		t.Fatalf("bin/ typeflag = %v, want TypeDir", d.hdr.Typeflag)
	}
	if h := entries[prefix+".hidden"]; string(h.body) != "h" {
		t.Fatalf(".hidden body = %q", h.body)
	}
}

func TestBottleWalkError(t *testing.T) {
	err := Bottle(filepath.Join(t.TempDir(), "nope"), project, version, &bytes.Buffer{})
	if err == nil {
		t.Fatal("want error for missing installDir")
	}
}

func TestBottleRelError(t *testing.T) {
	orig := walkDir
	defer func() { walkDir = orig }()
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return fn("not-absolute", nil, nil)
	}
	if err := Bottle(t.TempDir(), project, version, &bytes.Buffer{}); err == nil {
		t.Fatal("want filepath.Rel error")
	}
}

func TestBottleLstatError(t *testing.T) {
	orig := osLstat
	defer func() { osLstat = orig }()
	osLstat = func(string) (fs.FileInfo, error) { return nil, errBoom }
	if err := Bottle(makeTree(t), project, version, &bytes.Buffer{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestBottleReadlinkError(t *testing.T) {
	orig := osReadlink
	defer func() { osReadlink = orig }()
	osReadlink = func(string) (string, error) { return "", errBoom }
	if err := Bottle(makeTree(t), project, version, &bytes.Buffer{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestBottleFileInfoHeaderError(t *testing.T) {
	orig := tarFileInfoHeader
	defer func() { tarFileInfoHeader = orig }()
	tarFileInfoHeader = func(fs.FileInfo, string) (*tar.Header, error) { return nil, errBoom }
	if err := Bottle(makeTree(t), project, version, &bytes.Buffer{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestBottleWriteHeaderError(t *testing.T) {
	// Pinned to gzip: it writes its header EAGERLY on the first tar header
	// write, so a failing destination surfaces right there. zstd buffers, so it
	// is simply the wrong instrument for this assertion — the error would show
	// up somewhere else, or not until Close.
	withCodec(t, bottle.ExtTarGz)
	if err := Bottle(makeTree(t), project, version, errWriter{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestBottleOpenError(t *testing.T) {
	orig := osOpen
	defer func() { osOpen = orig }()
	osOpen = func(string) (*os.File, error) { return nil, errBoom }
	if err := Bottle(makeTree(t), project, version, &bytes.Buffer{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestBottleCopyError(t *testing.T) {
	orig := ioCopy
	defer func() { ioCopy = orig }()
	ioCopy = func(io.Writer, io.Reader) (int64, error) { return 0, errBoom }
	if err := Bottle(makeTree(t), project, version, &bytes.Buffer{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestBottleTarCloseError(t *testing.T) {
	// Pinned to gzip, same reason as TestBottleWriteHeaderError: an empty tree
	// writes nothing until tw.Close flushes the tar trailer, which triggers the
	// first (failing) write to the destination.
	withCodec(t, bottle.ExtTarGz)
	if err := Bottle(t.TempDir(), project, version, errWriter{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestWriteBottle(t *testing.T) {
	dir := makeTree(t)
	out := t.TempDir()

	p, err := WriteBottle(dir, project, version, "darwin", "aarch64", out)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(out, project, "darwin", "aarch64", "v"+version+Codec)
	if p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	order, _ := readBottle(t, f)
	if len(order) == 0 || order[0] != prefix+".hidden" {
		t.Fatalf("unexpected bottle contents: %v", order)
	}

	vtxt := filepath.Join(out, project, "darwin", "aarch64", "versions.txt")
	b, err := os.ReadFile(vtxt)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != version+"\n" {
		t.Fatalf("versions.txt = %q, want %q", b, version+"\n")
	}

	// A second call appends.
	if _, err := WriteBottle(dir, project, "3.0.0", "darwin", "aarch64", out); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(vtxt)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != version+"\n3.0.0\n" {
		t.Fatalf("versions.txt = %q, want %q", b, version+"\n3.0.0\n")
	}
}

func TestWriteBottleMkdirAllError(t *testing.T) {
	orig := osMkdirAll
	defer func() { osMkdirAll = orig }()
	osMkdirAll = func(string, fs.FileMode) error { return errBoom }
	if _, err := WriteBottle(makeTree(t), project, version, "darwin", "aarch64", t.TempDir()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestWriteBottleCreateError(t *testing.T) {
	orig := osCreate
	defer func() { osCreate = orig }()
	osCreate = func(string) (io.WriteCloser, error) { return nil, errBoom }
	if _, err := WriteBottle(makeTree(t), project, version, "darwin", "aarch64", t.TempDir()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestWriteBottleBottleError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := WriteBottle(missing, project, version, "darwin", "aarch64", t.TempDir()); err == nil {
		t.Fatal("want error for missing installDir")
	}
}

func TestWriteBottleTarballCloseError(t *testing.T) {
	orig := osCreate
	defer func() { osCreate = orig }()
	osCreate = func(string) (io.WriteCloser, error) { return &fakeWC{closeErr: errBoom}, nil }
	if _, err := WriteBottle(makeTree(t), project, version, "darwin", "aarch64", t.TempDir()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestWriteBottleVersionsOpenError(t *testing.T) {
	orig := osOpenFileAppend
	defer func() { osOpenFileAppend = orig }()
	osOpenFileAppend = func(string) (io.WriteCloser, error) { return nil, errBoom }
	if _, err := WriteBottle(makeTree(t), project, version, "darwin", "aarch64", t.TempDir()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestWriteBottleVersionsWriteError(t *testing.T) {
	orig := osOpenFileAppend
	defer func() { osOpenFileAppend = orig }()
	osOpenFileAppend = func(string) (io.WriteCloser, error) { return &fakeWC{writeErr: errBoom}, nil }
	if _, err := WriteBottle(makeTree(t), project, version, "darwin", "aarch64", t.TempDir()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestWriteBottleVersionsCloseError(t *testing.T) {
	orig := osOpenFileAppend
	defer func() { osOpenFileAppend = orig }()
	osOpenFileAppend = func(string) (io.WriteCloser, error) { return &fakeWC{closeErr: errBoom}, nil }
	if _, err := WriteBottle(makeTree(t), project, version, "darwin", "aarch64", t.TempDir()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}
