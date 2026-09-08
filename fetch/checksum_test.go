package fetch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseChecksum(t *testing.T) {
	const want = "b6a5f44b7eb69e3fa35dbf15524405b44837a481d43d81daddde3ff21fcbb8e9"
	cases := []struct {
		name, body, file, want string
	}{
		// openssl.org publishes the digest and nothing else.
		{"bare hex", want + "\n", "openssl-3.6.0.tar.gz", want},
		{"upper case", strings.ToUpper(want), "x.tar.gz", want},
		{"coreutils, one file", want + "  openssl-3.6.0.tar.gz\n", "openssl-3.6.0.tar.gz", want},
		{"bsd form", "SHA256 (openssl-3.6.0.tar.gz) = " + want, "openssl-3.6.0.tar.gz", want},
		// A SHA256SUMS covering a whole release: the LINE MATTERS. Taking the
		// first would verify the download against another file's digest.
		{"many files", "1111111111111111111111111111111111111111111111111111111111111111  other.tar.gz\n" +
			want + "  openssl-3.6.0.tar.gz\n", "https://x/openssl-3.6.0.tar.gz", want},
		{"binary star", "0000000000000000000000000000000000000000000000000000000000000000 *a.tgz\n" +
			want + " *b.tgz\n", "b.tgz", want},
		{"path-qualified", "0000000000000000000000000000000000000000000000000000000000000000  ./a.tgz\n" +
			want + "  ./b.tgz\n", "b.tgz", want},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseChecksum(c.body, c.file)
			if err != nil || got != c.want {
				t.Errorf("parseChecksum = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestParseChecksumRefusals(t *testing.T) {
	cases := []struct{ name, body, file, wantErr string }{
		{"empty", "  \n\n", "a.tgz", "empty"},
		{"one line, no digest", "<html>404</html>", "a.tgz", "no sha-256"},
		// The dangerous case: a real checksum file that simply does not cover
		// the archive we downloaded. Silence here would be a pass.
		{"names another file", "1111111111111111111111111111111111111111111111111111111111111111  a.tgz\n" +
			"2222222222222222222222222222222222222222222222222222222222222222  b.tgz\n", "c.tgz", "no line names c.tgz"},
		{"many lines, no digest at all", "not a checksum\nfile at all\n", "a.tgz", "no line names"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseChecksum(c.body, c.file); err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v; want %q", err, c.wantErr)
			}
		})
	}
}

func TestDeclaredSHA256(t *testing.T) {
	restoreSeams(t)
	const want = "b6a5f44b7eb69e3fa35dbf15524405b44837a481d43d81daddde3ff21fcbb8e9"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.sha256":
			_, _ = w.Write([]byte(want + "\n"))
		case "/html.sha256":
			_, _ = w.Write([]byte("<!doctype html><title>Not found</title>"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)

	got, err := DeclaredSHA256(s.URL+"/ok.sha256", "openssl-3.6.0.tar.gz")
	if err != nil || got != want {
		t.Errorf("DeclaredSHA256 = %q, %v", got, err)
	}
	if _, err := DeclaredSHA256(s.URL+"/missing.sha256", "a.tgz"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: err = %v, want the status", err)
	}
	// A host that answers 200 with an error page is the failure that otherwise
	// blames the format; describeBody says what actually arrived.
	if _, err := DeclaredSHA256(s.URL+"/html.sha256", "a.tgz"); err == nil || !strings.Contains(err.Error(), "no sha-256") {
		t.Errorf("html: err = %v", err)
	}
	httpGet = func(string) (*http.Response, error) { return nil, errors.New("boom") }
	if _, err := DeclaredSHA256(s.URL+"/ok.sha256", "a.tgz"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("transport: err = %v", err)
	}
}

// A body that cannot be read to the end fails rather than being parsed as far
// as it got — half a checksum file is not a checksum file.
func TestDeclaredSHA256ReadError(t *testing.T) {
	restoreSeams(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		_, _ = w.Write([]byte("short"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-body
	}))
	t.Cleanup(s.Close)
	if _, err := DeclaredSHA256(s.URL+"/x.sha256", "a.tgz"); err == nil || !strings.Contains(err.Error(), "read checksum") {
		t.Errorf("err = %v, want the read failure", err)
	}
}
