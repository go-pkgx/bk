package fetch

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain silences the retry backoff for the whole package: the real schedule
// spans three seconds, and no test here is measuring wall-clock.
func TestMain(m *testing.M) {
	sleepFn = func(time.Duration) {}
	os.Exit(m.Run())
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// TestDownloadRetriesATimeout: the shape that lost seven x.org recipes and
// gnu.org/gmp in one batch — a dial that never completes. The second attempt
// succeeds, so the download does.
func TestDownloadRetriesATimeout(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, &net.OpError{Op: "dial", Err: timeoutErr{}}
		}
		return okBody(gzMagic + "hello"), nil
	}
	path, err := download("https://flaky.example/x.tar.gz")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer os.Remove(path)
	if calls != 2 {
		t.Errorf("attempts=%d, want 2", calls)
	}
	if b, _ := os.ReadFile(path); string(b) != gzMagic+"hello" {
		t.Errorf("body=%q", b)
	}
}

// TestDownloadRetriesA503: freetype.org answered 502 and ijg.org 503; both were
// recorded as failed recipes.
func TestDownloadRetriesA503(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) {
		calls++
		if calls < 3 {
			return &http.Response{StatusCode: 503, Status: "503 Service Unavailable", Body: http.NoBody}, nil
		}
		return okBody(gzMagic + "data"), nil
	}
	path, err := download("https://flaky.example/x.tar.gz")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer os.Remove(path)
	if calls != 3 {
		t.Errorf("attempts=%d, want 3", calls)
	}
}

// TestDownloadDoesNotRetryA404: a 4xx is the server answering. Repeating it
// wastes the attempts and buries the message.
func TestDownloadDoesNotRetryA404(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: http.NoBody}, nil
	}
	if _, err := download("https://example.invalid/x.tar.gz"); err == nil {
		t.Fatal("expected the 404 to fail the download")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("the status is not reported: %v", err)
	}
	if calls != 1 {
		t.Errorf("attempts=%d, want 1 — a 404 must not be retried", calls)
	}
}

// TestDownloadGivesUpAndSaysWhy: every attempt transient. The final error names
// the last real failure rather than claiming a truncation that never happened.
func TestDownloadGivesUpAndSaysWhy(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) {
		calls++
		return nil, &net.OpError{Op: "dial", Err: timeoutErr{}}
	}
	_, err := download("https://dead.example/x.tar.gz")
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "truncated") {
		t.Errorf("the error blames the wrong thing: %v", err)
	}
	if calls != downloadAttempts {
		t.Errorf("attempts=%d, want %d", calls, downloadAttempts)
	}
}

// TestDownloadStillFailsOnAPermanentTransportError: a URL the client refuses is
// not worth repeating.
func TestDownloadStillFailsOnAPermanentTransportError(t *testing.T) {
	defer restoreHTTPGet(httpGet)
	calls := 0
	httpGet = func(string) (*http.Response, error) {
		calls++
		return nil, errors.New("unsupported protocol scheme")
	}
	if _, err := download("wat://x"); err == nil {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Errorf("attempts=%d, want 1", calls)
	}
}

func okBody(s string) *http.Response {
	return &http.Response{
		StatusCode:    200,
		Status:        "200 OK",
		Body:          io.NopCloser(strings.NewReader(s)),
		ContentLength: int64(len(s)),
	}
}

func restoreHTTPGet(f func(string) (*http.Response, error)) { httpGet = f }

// gzMagic is what a .tar.gz must begin with. The retry fixtures carry it so the
// magic check (see magic_test.go) does not treat them as a mirror that served
// the wrong thing — which is exactly what it is for.
const gzMagic = "\x1f\x8b"
