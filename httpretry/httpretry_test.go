package httpretry

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type notTimeout struct{}

func (notTimeout) Error() string   { return "refused by policy" }
func (notTimeout) Timeout() bool   { return false }
func (notTimeout) Temporary() bool { return false }

func TestTransient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   bool
	}{
		// The shapes measured in one 23-failure batch.
		{"dial timeout (x.org, gmp, pkg-config)", &net.OpError{Op: "dial", Err: timeoutErr{}}, 0, true},
		{"502 (freetype.org)", nil, http.StatusBadGateway, true},
		{"503 (ijg.org)", nil, http.StatusServiceUnavailable, true},
		{"429", nil, http.StatusTooManyRequests, true},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), 0, true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), 0, true},
		{"broken pipe", fmt.Errorf("write: %w", syscall.EPIPE), 0, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, 0, true},
		{"EOF", io.EOF, 0, true},
		// Answers, not accidents.
		{"404", nil, http.StatusNotFound, false},
		{"403", nil, http.StatusForbidden, false},
		{"200", nil, http.StatusOK, false},
		{"bad scheme", errors.New("unsupported protocol scheme"), 0, false},
		{"a net.Error that is not a timeout", &net.OpError{Op: "dial", Err: notTimeout{}}, 0, false},
	} {
		if got := Transient(tc.err, tc.status); got != tc.want {
			t.Errorf("%s: Transient=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	for i, w := range want {
		if got := Backoff(i); got != w {
			t.Errorf("Backoff(%d)=%v, want %v", i, got, w)
		}
	}
}
