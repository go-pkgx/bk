// Package httpretry decides which failed HTTP attempts are worth repeating.
//
// It exists because a single unlucky draw against a flaky host loses a recipe
// for a whole factory run. Measured on one 23-failure batch, ELEVEN were
// transient HTTP and nothing else:
//
//	x.org (×7)          dial tcp 131.252.210.176:443: i/o timeout
//	gnu.org/gmp         dial tcp 130.242.124.102:443: i/o timeout
//	pkg-config          dial tcp 131.252.210.176:443: i/o timeout
//	freetype.org        502 Bad Gateway
//	ijg.org             503 Service Temporarily Unavailable
//
// xorg.freedesktop.org is not down — three consecutive requests from a second
// machine answered 200 in 8.2s, 200 in 2.4s, then timed out. It is flaky, and
// a factory that gives a flaky host exactly one chance reports its own bad
// luck as a broken recipe.
//
// The distinction that matters is transient versus answered. A 403 or a 404 is
// the server telling you something true; repeating it wastes time and hides
// the real message. A timeout, a reset, or a 5xx is the server failing to
// answer at all.
package httpretry

import (
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Attempts is how many times a transient failure is retried before giving up.
// Three attempts spans ~3s of backoff, which covers the flakiness measured
// above without turning a genuinely dead host into a slow build.
const Attempts = 3

// Transient reports whether an attempt failed in a way worth repeating. Pass
// the transport error (nil if the request completed) and the status code (0 if
// it did not).
func Transient(err error, status int) bool {
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return true
		}
		// A connection cut or refused mid-flight is the same kind of accident as
		// a timeout: nothing about the request was rejected.
		if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrUnexpectedEOF) ||
			errors.Is(err, io.EOF) {
			return true
		}
		// Anything else — a malformed URL, an unsupported scheme, a TLS identity
		// that does not check out — is an answer too, just from our own side.
		return false
	}
	// 429 is an explicit "later"; 5xx is the server failing to answer. Every
	// 4xx below 429 is an answer about the request itself.
	return status == http.StatusTooManyRequests || status >= 500
}

// Backoff is how long to wait before attempt n, counting from 0: 500ms, 1s, 2s.
func Backoff(n int) time.Duration {
	d := 500 * time.Millisecond
	for i := 0; i < n; i++ {
		d *= 2
	}
	return d
}
