package versions

import (
	"errors"
	"strings"
	"testing"
)

// TestDescribeRemoteErr: a recipe pointing at a repository that has gone away
// fails as "authentication required", which reads as a credentials problem in
// CI and sends the reader to look at tokens. Six of eighty-one failures in one
// factory run wore that label; every one was a pantry recipe whose upstream had
// been deleted or renamed.
func TestDescribeRemoteErr(t *testing.T) {
	for _, msg := range []string{
		"authentication required: Repository not found.",
		"Authentication Required",
		"repository not found",
	} {
		got := describeRemoteErr(errors.New(msg))
		if !strings.Contains(got.Error(), "does not exist, or is private") {
			t.Errorf("%q was not explained: %v", msg, got)
		}
		// The original text survives: it is what the host actually said, and a
		// reader matching on it should still find it.
		if !strings.Contains(got.Error(), msg) {
			t.Errorf("%q lost the original wording: %v", msg, got)
		}
	}

	// Everything else is passed through untouched — a TLS failure or a
	// deadline is not a missing repository, and dressing it up as one would be
	// the same mistake in the other direction.
	for _, msg := range []string{
		"tls: failed to verify certificate",
		"context deadline exceeded",
		"unexpected EOF",
	} {
		in := errors.New(msg)
		if got := describeRemoteErr(in); got != in {
			t.Errorf("%q was rewritten: %v", msg, got)
		}
	}
	if describeRemoteErr(nil) != nil {
		t.Error("nil must stay nil")
	}
	// Unwrapping still reaches the cause.
	base := errors.New("authentication required")
	if !errors.Is(describeRemoteErr(base), base) {
		t.Error("the cause is no longer reachable with errors.Is")
	}
}
