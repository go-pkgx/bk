package fetch

import (
	"strings"
	"testing"
)

// TestFetchBz2ErrorPageSaysWhatArrived is the pcre.org 8.45 failure. A mirror
// answered 200 with a page, and the whole factory run recorded:
//
//	fetch: bzip2 data invalid: bad magic value
//
// which names neither the URL nor the cause. compress/bzip2 has no eager
// constructor, so the corruption surfaces mid-extract and the message used to
// come straight out of the tar reader.
func TestFetchBz2ErrorPageSaysWhatArrived(t *testing.T) {
	s := serve(t, []byte("<!DOCTYPE html><html><body>404 Not Found</body></html>"))
	err := Fetch(s.URL+"/pcre-8.45.tar.bz2", t.TempDir(), 0)
	if err == nil {
		t.Fatal("expected the HTML page to fail the fetch")
	}
	msg := err.Error()
	for _, want := range []string{"read bzip2 from", "pcre-8.45.tar.bz2", "an HTML page", "404 Not Found"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not carry %q:\n%s", want, msg)
		}
	}
}

// TestFetchTarErrorPageSaysWhatArrived: plain tar has the same shape — no
// constructor to reject the stream.
func TestFetchTarErrorPageSaysWhatArrived(t *testing.T) {
	s := serve(t, []byte("Not Found\n"))
	err := Fetch(s.URL+"/x.tar", t.TempDir(), 0)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "read tar from") || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("message: %s", err)
	}
}

// TestFetchZipErrorPageSaysWhatArrived: and zip.
func TestFetchZipErrorPageSaysWhatArrived(t *testing.T) {
	s := serve(t, []byte("<html>gone</html>"))
	err := Fetch(s.URL+"/x.zip", t.TempDir(), 0)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "read zip from") || !strings.Contains(err.Error(), "an HTML page") {
		t.Errorf("message: %s", err)
	}
}

// TestExtractFailureOnARealArchiveIsNotBlamedOnTheServer: the head IS a bzip2
// stream, so the failure is about the archive or the disk. Dressing that up as
// "the server returned an HTML page" would send the reader the wrong way.
func TestExtractFailureOnARealArchiveIsNotBlamedOnTheServer(t *testing.T) {
	// A valid bzip2 header followed by rubbish: high-entropy, so describeBody
	// must stay out of it.
	body := append([]byte("BZh9"), []byte{0x31, 0x41, 0x59, 0x26, 0x53, 0x59, 0xde, 0xad, 0xbe, 0xef, 0x7f, 0x91, 0xa3, 0xc5}...)
	s := serve(t, body)
	err := Fetch(s.URL+"/x.tar.bz2", t.TempDir(), 0)
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "the server returned") {
		t.Errorf("a real archive's failure was blamed on the server:\n%s", err)
	}
	if !strings.Contains(err.Error(), "read bzip2 from") {
		t.Errorf("the URL is still missing from the message:\n%s", err)
	}
}

func TestDescribeIfNotArchive(t *testing.T) {
	if got := describeIfNotArchive(nil); !strings.Contains(got, "EMPTY") {
		t.Errorf("empty body: %q", got)
	}
	if got := describeIfNotArchive([]byte("<html>hi</html>")); !strings.Contains(got, "an HTML page") {
		t.Errorf("html: %q", got)
	}
	if got := describeIfNotArchive([]byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad, 0xbe, 0xef, 0x7f, 0x91}); got != "" {
		t.Errorf("a binary head must not be described: %q", got)
	}
}

func TestWrapExtractPassesNilThrough(t *testing.T) {
	if err := wrapExtract(nil, "bzip2", "http://x/y.tar.bz2", []byte("<html>")); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}
