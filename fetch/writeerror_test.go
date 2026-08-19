package fetch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestDownloadStopsOnAWriteError is the real incident, in miniature. A CI
// runner's disk filled while fetching kernel tarballs, and every subsequent
// download reported itself as a TRUNCATED TRANSFER:
//
//	fetch: write .../build/.clang-format: no space left on device
//	fetch: GET .../linux-6.13.9.tar.xz: still truncated after 5 attempts
//
// Same runner, minutes apart. The bytes had arrived; there was nowhere to put
// them — and the message sent the reader looking at the network.
func TestDownloadStopsOnAWriteError(t *testing.T) {
	body := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	attempts := 0
	orig := ioCopyBody
	ioCopyBody = func(io.Writer, io.Reader) (int64, error) {
		attempts++
		return 0, &fs.PathError{Op: "write", Path: "/tmp/x", Err: syscall.ENOSPC}
	}
	defer func() { ioCopyBody = orig }()

	_, err := download(srv.URL + "/linux.tar.xz")

	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "truncated") {
		t.Errorf("a full disk was reported as a truncated transfer: %v", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("the real cause is not in the message: %v", err)
	}
	if attempts != 1 {
		t.Errorf("retried a write failure %d times — the server cannot fix a full disk", attempts)
	}
}

// TestDownloadStillResumesATruncatedBody: the change must not cost the resume
// behaviour it sits next to. A short body with no error is still a cut
// transfer, and still worth retrying.
func TestDownloadStillResumesATruncatedBody(t *testing.T) {
	full := strings.Repeat("y", 2000)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		start := 0
		if rng := r.Header.Get("Range"); rng != "" {
			_, _ = fmtSscanf(rng, &start)
			w.Header().Set("Content-Length", itoa(len(full)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", itoa(len(full)))
		}
		out := full[start:]
		if hits == 1 {
			out = out[:500] // cut the first response
		}
		_, _ = io.WriteString(w, out)
	}))
	defer srv.Close()

	p, err := download(srv.URL + "/x.tar.gz")
	if err != nil {
		t.Fatalf("a cut transfer must still be resumed: %v", err)
	}
	defer os.Remove(p)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != full {
		t.Errorf("resumed download is %d bytes, want %d", len(b), len(full))
	}
	if hits < 2 {
		t.Errorf("the transfer was not resumed (%d request)", hits)
	}
}

// TestIsWriteError names what counts. Network errors must NOT match, or a real
// cut transfer would stop being retried.
func TestIsWriteError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"disk full", syscall.ENOSPC, true},
		{"quota", syscall.EDQUOT, true},
		{"read-only fs", syscall.EROFS, true},
		{"path error", &fs.PathError{Op: "write", Err: syscall.ENOSPC}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, false},
		{"connection reset", syscall.ECONNRESET, false},
		{"plain error", errors.New("boom"), false},
	} {
		if got := isWriteError(tc.err); got != tc.want {
			t.Errorf("%s: isWriteError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// small helpers so the test server can speak Range without pulling fmt/strconv
// into the assertions above.
func fmtSscanf(rng string, start *int) (int, error) {
	var n int
	_, err := fmt.Sscanf(rng, "bytes=%d-", &n)
	*start = n
	return n, err
}

func itoa(n int) string { return strconv.Itoa(n) }
