package fetch

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDownloadRetriesAMirrorThatServedAPage is the pcre.org 8.45 shape: the
// transfer completes, the status is 200, and what arrived is not the archive.
// Both of pcre's URLs serve a good BZh9 tarball from elsewhere — SourceForge
// simply handed the runner something else that day — so it is a transient, and
// it was the one shape the retry loop could not see.
func TestDownloadRetriesAMirrorThatServedAPage(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return okBody("<!DOCTYPE html><html>mirror selection</html>"), nil
		}
		return okBody("BZh9real-enough"), nil
	}
	path, err := download("https://sf.example/pcre-8.45.tar.bz2")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer os.Remove(path)
	if calls != 2 {
		t.Fatalf("attempts=%d, want 2", calls)
	}
	if b, _ := os.ReadFile(path); !strings.HasPrefix(string(b), "BZh") {
		t.Errorf("kept the wrong body: %q", b)
	}
}

// TestDownloadKeepsAGoodArchive is the control: correct magic is returned on
// the first attempt, with no retry.
func TestDownloadKeepsAGoodArchive(t *testing.T) {
	for _, tc := range []struct{ url, body string }{
		{"https://x/a.tar.bz2", "BZh9payload"},
		{"https://x/a.tar.gz", "\x1f\x8bpayload"},
		{"https://x/a.tar.xz", "\xfd7zXZ\x00payload"},
		{"https://x/a.zip", "PKpayload"},
		// A plain .tar is not magic-checked: its first bytes are an ASCII
		// filename, indistinguishable from a page by any cheap test.
		{"https://x/a.tar", "whatever this is"},
		// An extension with no known magic is left alone too.
		{"https://x/a.bin", "whatever"},
	} {
		defer restoreHTTPGet(httpGet)
		calls := 0
		httpGet = func(string) (*http.Response, error) { calls++; return okBody(tc.body), nil }
		path, err := download(tc.url)
		if err != nil && !strings.Contains(err.Error(), "unknown archive") {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if path != "" {
			os.Remove(path)
		}
		if calls != 1 {
			t.Errorf("%s: attempts=%d, want 1", tc.url, calls)
		}
	}
}

// TestDownloadGivesUpOnAMirrorThatNeverServesTheArchive: every attempt wrong,
// so the last body is kept and the extractor gets to say what it is — which,
// since #61, it does by name.
func TestDownloadGivesUpOnAMirrorThatNeverServesTheArchive(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) { calls++; return okBody("<html>nope</html>"), nil }
	path, err := download("https://sf.example/x.tar.bz2")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer os.Remove(path)
	if calls != downloadAttempts {
		t.Errorf("attempts=%d, want %d", calls, downloadAttempts)
	}
}

func TestMagicWrong(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Shorter than the magic itself is certainly not the archive.
	if !magicWrong("https://x/a.tar.bz2", short) {
		t.Error("a one-byte body must count as wrong")
	}
	// An unreadable file is a different problem, left to the extractor.
	if magicWrong("https://x/a.tar.bz2", filepath.Join(dir, "missing")) {
		t.Error("an unreadable path must not be reported as wrong magic")
	}
}

// TestDownloadMagicRetryCannotMakeATempFile: the retry needs a fresh file to
// start over on, and if it cannot get one the failure must be the temp file,
// not a bogus "the archive is corrupt".
func TestDownloadMagicRetryCannotMakeATempFile(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	httpGet = func(string) (*http.Response, error) { return okBody("<html>page</html>"), nil }
	defer restoreCreateTemp(osCreateTemp)
	n := 0
	real := osCreateTemp
	osCreateTemp = func(dir, pat string) (*os.File, error) {
		n++
		if n > 1 {
			return nil, os.ErrPermission
		}
		return real(dir, pat)
	}
	_, err := download("https://sf.example/x.tar.bz2")
	if err == nil || !strings.Contains(err.Error(), "temp file") {
		t.Fatalf("got %v, want a temp-file error", err)
	}
}

func restoreCreateTemp(f func(string, string) (*os.File, error)) { osCreateTemp = f }
