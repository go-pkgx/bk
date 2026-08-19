package fetch

import (
	"archive/tar"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestFetchSaysWhatTheServerActuallySent is the diagnosis the old message
// withheld. zlib.net answered a build with something that was not a tarball and
// the run reported:
//
//	fetch: read gzip from https://zlib.net/zlib-1.3.2.tar.gz: gzip: invalid header
//
// which reads as a corrupt archive and sends the reader to check the tarball.
// The archive was never there.
func TestFetchSaysWhatTheServerActuallySent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html>\n<html><body><h1>404 Not Found</h1></body></html>"))
	}))
	defer srv.Close()

	err := Fetch(srv.URL+"/zlib-1.3.2.tar.gz", t.TempDir(), 1)

	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTML page") {
		t.Errorf("the message does not say what arrived: %v", err)
	}
	if !strings.Contains(msg, "404 Not Found") {
		t.Errorf("the message does not quote the body: %v", err)
	}
	// The original cause stays: it is still true, just no longer the whole story.
	if !strings.Contains(msg, "invalid header") {
		t.Errorf("the underlying error was dropped: %v", err)
	}
}

// TestFetchStillWorksOnARealArchive: the sniff reads the head and must rewind,
// or every download breaks in the name of a better error message.
func TestFetchStillWorksOnARealArchive(t *testing.T) {
	payload := gzWrap(t, buildTar(t, []tarEntry{
		{name: "pkg-1.0/README", typ: tar.TypeReg, mode: 0o644, body: "hello"},
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	dest := t.TempDir()

	if err := Fetch(srv.URL+"/pkg-1.0.tar.gz", dest, 1); err != nil {
		t.Fatalf("a valid archive must still extract: %v", err)
	}
	b, err := os.ReadFile(dest + "/README")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Errorf("extracted %q", b)
	}
}

func TestDescribeBody(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", "EMPTY body"},
		{"html", "<!DOCTYPE html><html>oops</html>", "an HTML page"},
		{"xml", `<?xml version="1.0"?><error/>`, "an XML document"},
		{"plain text", "Not Found\n", "the server returned text"},
	} {
		if got := describeBody([]byte(tc.in)); !strings.Contains(got, tc.want) {
			t.Errorf("%s: describeBody = %q, want it to mention %q", tc.name, got, tc.want)
		}
	}
	// Binary that is simply not the right archive: quote the bytes, do not
	// pretend to know what it is.
	got := describeBody([]byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x80, 0x7f, 0x03, 0x04})
	if !strings.Contains(got, "first bytes:") {
		t.Errorf("binary body: %q", got)
	}
}

// TestDescribeBodyCollapsesWhitespace keeps a multi-line error page readable on
// one log line.
func TestDescribeBodyCollapsesWhitespace(t *testing.T) {
	got := describeBody([]byte("Error:\n\n   the mirror is down\n"))
	if strings.Contains(got, "\n") {
		t.Errorf("the snippet spans lines: %q", got)
	}
	if !strings.Contains(got, "Error: the mirror is down") {
		t.Errorf("got %q", got)
	}
}

// TestDescribeBodyTruncates: a whole HTML page in an error message is not a
// message, it is a dump.
func TestDescribeBodyTruncates(t *testing.T) {
	got := describeBody([]byte("<html>" + strings.Repeat("a", 400)))
	if len(got) > 260 {
		t.Errorf("message is %d chars: %q", len(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("no truncation marker: %q", got)
	}
}

// TestIsMostlyText draws the line the message depends on: compressed data is
// high-entropy bytes, an error page is not.
func TestIsMostlyText(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, false},
		{"ascii", []byte("Not Found"), true},
		{"utf-8 accents", []byte("Erreur : le miroir est indisponible"), true},
		{"gzip header", []byte{0x1f, 0x8b, 0x08, 0x00, 0xf9, 0x63, 0x94, 0x69}, false},
		{"invalid utf-8", []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8}, false},
		{"text with a stray byte", append([]byte(strings.Repeat("hello world ", 8)), 0xff), true},
	} {
		if got := isMostlyText(tc.in); got != tc.want {
			t.Errorf("%s: isMostlyText = %v, want %v", tc.name, got, tc.want)
		}
	}
}
