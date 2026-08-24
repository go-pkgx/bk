package versions

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

// TestMain silences the retry backoff for the package.
func TestMain(m *testing.M) {
	sleepFn = func(time.Duration) {}
	os.Exit(m.Run())
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// TestHTTPDoRetryingSurvivesAFlakyHost is the x.org case: the listing host
// answers on the second try, and seven recipes stop being reported as having
// no candidate version.
func TestHTTPDoRetryingSurvivesAFlakyHost(t *testing.T) {
	defer restoreDo(httpDo)
	calls := 0
	httpDo = func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, &net.OpError{Op: "dial", Err: timeoutErr{}}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}
	req, _ := http.NewRequest(http.MethodGet, "https://flaky.example/", nil)
	resp, err := httpDoRetrying(req)
	if err != nil {
		t.Fatalf("httpDoRetrying: %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Errorf("attempts=%d, want 2", calls)
	}
}

// TestHTTPDoRetryingRetries5xxAndClosesTheBody: a 5xx response has a body, and
// abandoning it without closing leaks the connection.
func TestHTTPDoRetryingRetries5xxAndClosesTheBody(t *testing.T) {
	defer restoreDo(httpDo)
	calls, closed := 0, 0
	httpDo = func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 502, Body: countingCloser{&closed}}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}
	req, _ := http.NewRequest(http.MethodGet, "https://flaky.example/", nil)
	resp, err := httpDoRetrying(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 || closed != 1 {
		t.Errorf("attempts=%d bodiesClosed=%d, want 2 and 1", calls, closed)
	}
}

// TestHTTPDoRetryingKeepsA404: an answer is an answer, returned on the first
// attempt with its body intact for the caller to read.
func TestHTTPDoRetryingKeepsA404(t *testing.T) {
	defer restoreDo(httpDo)
	calls := 0
	httpDo = func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("nope"))}, nil
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	resp, err := httpDoRetrying(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 1 || resp.StatusCode != 404 {
		t.Errorf("attempts=%d status=%d, want 1 and 404", calls, resp.StatusCode)
	}
}

// TestHTTPDoRetryingGivesUp: every attempt transient, the last error surfaces.
func TestHTTPDoRetryingGivesUp(t *testing.T) {
	defer restoreDo(httpDo)
	calls := 0
	boom := &net.OpError{Op: "dial", Err: timeoutErr{}}
	httpDo = func(*http.Request) (*http.Response, error) {
		calls++
		return nil, boom
	}
	req, _ := http.NewRequest(http.MethodGet, "https://dead.example/", nil)
	if _, err := httpDoRetrying(req); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if calls != 3 {
		t.Errorf("attempts=%d, want 3", calls)
	}
}

type countingCloser struct{ n *int }

func (c countingCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (c countingCloser) Close() error             { *c.n++; return nil }

func restoreDo(f func(*http.Request) (*http.Response, error)) { httpDo = f }
