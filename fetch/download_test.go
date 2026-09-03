package fetch

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestDownloadServerIgnoresRange: a server that ignores the Range header and
// replays the WHOLE body must not have that body appended to what we already
// stored — the download starts over on a fresh file.
func TestDownloadServerIgnoresRange(t *testing.T) {
	restoreSeams(t)
	full := []byte("0123456789abcdefghij")
	var n int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		if n == 1 {
			_, _ = w.Write(full[:5]) // short, and no 206 on the retry
			return
		}
		_, _ = w.Write(full) // ignores Range: replays everything
	}))
	t.Cleanup(s.Close)

	path, err := download(s.URL + "/pkg.tar")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Errorf("got %q, want exactly %q — a replayed body was appended", got, full)
	}
}

// A second temp file is needed for that restart; when it cannot be created the
// failure is reported rather than papered over.
func TestDownloadRestartTempFileError(t *testing.T) {
	restoreSeams(t)
	full := []byte("0123456789")
	var n int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		if n == 1 {
			_, _ = w.Write(full[:3])
			return
		}
		_, _ = w.Write(full)
	}))
	t.Cleanup(s.Close)

	calls := 0
	real := os.CreateTemp
	osCreateTemp = func(dir, pattern string) (*os.File, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("boom")
		}
		return real(dir, pattern)
	}
	if _, err := download(s.URL + "/pkg.tar"); err == nil || !strings.Contains(err.Error(), "temp file") {
		t.Errorf("err = %v, want the temp-file failure", err)
	}
}

// Every other error branch: the first temp file, the transport, and a status
// that is neither 200 nor 206.
func TestDownloadErrors(t *testing.T) {
	restoreSeams(t)
	boom := errors.New("boom")

	osCreateTemp = func(string, string) (*os.File, error) { return nil, boom }
	if _, err := download("http://x/y.tar"); err == nil || !strings.Contains(err.Error(), "temp file") {
		t.Errorf("temp file error = %v", err)
	}
	osCreateTemp = os.CreateTemp

	httpGet = func(string) (*http.Response, error) { return nil, boom }
	if _, err := download("http://x/y.tar"); err == nil || !strings.Contains(err.Error(), "GET") {
		t.Errorf("transport error = %v", err)
	}
	httpGet = getWithAgent

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	t.Cleanup(s.Close)
	if _, err := download(s.URL + "/y.tar"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("status error = %v", err)
	}
}

// The Range request can fail to build (a URL the client rejects) or to travel;
// both are reported rather than retried into silence.
func TestHTTPGetRangeErrors(t *testing.T) {
	restoreSeams(t)
	if _, err := httpGetRange("http://[::1]:namedport/x", 10); err == nil {
		t.Error("expected an error for an unbuildable request")
	}
	httpDo = func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }
	if _, err := httpGetRange("http://x/y", 10); err == nil {
		t.Error("expected the transport error")
	}
	httpDo = doWithAgent
}

// The downloaded file has to be reopened for extraction; when that fails the
// fetch says so instead of extracting nothing and reporting success.
func TestFetchReopenError(t *testing.T) {
	restoreSeams(t)
	s := serve(t, gzWrap(t, sampleTar(t)))
	osOpen = func(string) (*os.File, error) { return nil, errors.New("boom") }
	if err := Fetch(s.URL+"/pkg.tar.gz", t.TempDir(), 0); err == nil || !strings.Contains(err.Error(), "open") {
		t.Errorf("err = %v, want the reopen failure", err)
	}
}

// A zip whose bytes cannot be read back is reported too (the read now happens
// from the temp file, not from the network body).
func TestFetchZipReadFileError(t *testing.T) {
	restoreSeams(t)
	s := serve(t, buildZip(t, []zipEntry{{name: "a.txt", mode: 0o644, body: "x"}}))
	ioCopy = func(io.Writer, io.Reader) (int64, error) { return 0, errors.New("copy boom") }
	// io.ReadAll on a closed file: close it as soon as it is opened.
	osOpen = func(name string) (*os.File, error) {
		f, err := os.Open(name)
		if err == nil {
			f.Close()
		}
		return f, err
	}
	if err := Fetch(s.URL+"/pkg.zip", t.TempDir(), 0); err == nil || !strings.Contains(err.Error(), "read body") {
		t.Errorf("err = %v, want the read-body failure", err)
	}
}
