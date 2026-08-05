package versions

import (
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

var errBoom = errors.New("boom")

// saveSeams snapshots every injectable seam and restores it after the test.
func saveSeams(t *testing.T) {
	t.Helper()
	hg, glt, hgr, ira, ec := httpGet, gitLsRemoteTags, httpGetRaw, ioReadAll, execCommand
	t.Cleanup(func() {
		httpGet, gitLsRemoteTags, httpGetRaw, ioReadAll, execCommand = hg, glt, hgr, ira, ec
	})
}

// TestResolveURLNcurses is the end-to-end proof: an ftp.gnu.org-style listing
// must yield 6.5 (the real upstream file), NOT dist's normalised 6.6.0.
func TestResolveURLNcurses(t *testing.T) {
	saveSeams(t)
	listing := `<a href="ncurses-6.3.tar.gz">ncurses-6.3.tar.gz</a>
<a href="ncurses-6.4.tar.gz">ncurses-6.4.tar.gz</a>
<a href="ncurses-6.5.tar.gz">ncurses-6.5.tar.gz</a>`
	httpGet = func(string) (string, error) { return listing, nil }
	spec := map[string]any{
		"url":   "https://ftp.gnu.org/gnu/ncurses/",
		"match": `/ncurses-\d+(\.\d+)+.tar.gz/`,
		"strip": []any{"/ncurses-/", "/.tar.gz/"},
	}
	v, tag, err := Resolve(spec, "*")
	if err != nil || v != "6.5" {
		t.Fatalf("Resolve = %q, %v; want 6.5", v, err)
	}
	// version.tag is the raw pre-strip matched string (upstream file name).
	if tag != "ncurses-6.5.tar.gz" {
		t.Fatalf("tag = %q; want ncurses-6.5.tar.gz", tag)
	}
}

// TestResolveGithub proves github tags select the semver (not lexical) max.
func TestResolveGithub(t *testing.T) {
	saveSeams(t)
	var gotURL string
	gitLsRemoteTags = func(repoURL string) ([]string, error) {
		gotURL = repoURL
		return []string{"v1.2.0", "v1.10.0", "v1.3.0"}, nil
	}
	spec := map[string]any{"github": "owner/repo", "strip": "/v/"}
	v, tag, err := Resolve(spec, "")
	if err != nil || v != "1.10.0" {
		t.Fatalf("Resolve = %q, %v; want 1.10.0", v, err)
	}
	// version.tag = the raw git tag (v-prefixed), not the stripped version.
	if tag != "v1.10.0" {
		t.Fatalf("tag = %q; want v1.10.0", tag)
	}
	if gotURL != "https://github.com/owner/repo" {
		t.Errorf("repoURL = %q", gotURL)
	}
}

// TestResolveGithubNonVTag proves a non-"v" upstream tag (e.g. openssl-3.5.0)
// is preserved verbatim as version.tag while the version strips to 3.5.0.
func TestResolveGithubNonVTag(t *testing.T) {
	saveSeams(t)
	gitLsRemoteTags = func(string) ([]string, error) {
		return []string{"openssl-3.4.0", "openssl-3.5.0"}, nil
	}
	spec := map[string]any{"github": "openssl/openssl", "strip": "/openssl-/"}
	v, tag, err := Resolve(spec, "*")
	if err != nil || v != "3.5.0" {
		t.Fatalf("Resolve = %q, %v; want 3.5.0", v, err)
	}
	if tag != "openssl-3.5.0" {
		t.Fatalf("tag = %q; want openssl-3.5.0", tag)
	}
}

func TestResolveGithubDropsVPrefix(t *testing.T) {
	saveSeams(t)
	// github tags carry a leading "v" and the recipe has no strip → the result
	// is normalised to the pkgx tag form (no v), matching dist bottle tags.
	gitLsRemoteTags = func(string) ([]string, error) {
		return []string{"v5.8.2", "v5.8.3"}, nil
	}
	v, tag, err := Resolve(map[string]any{"github": "tukaani-project/xz"}, "*")
	if err != nil || v != "5.8.3" {
		t.Fatalf("Resolve = %q, %v; want 5.8.3 (v stripped)", v, err)
	}
	// The version drops the v, but version.tag keeps the real git tag.
	if tag != "v5.8.3" {
		t.Fatalf("tag = %q; want v5.8.3", tag)
	}
}

// TestResolveGithubSuffixForms accepts every "list tags" spelling.
func TestResolveGithubSuffixForms(t *testing.T) {
	saveSeams(t)
	gitLsRemoteTags = func(string) ([]string, error) {
		return []string{"refs/tags/1.0.0", "refs/tags/1.1.0"}, nil
	}
	for _, gh := range []string{"o/r", "o/r/tags", "o/r/releases", "o/r/releases/tags"} {
		v, tag, err := Resolve(map[string]any{"github": gh}, "*")
		if err != nil || v != "1.1.0" {
			t.Errorf("%s: Resolve = %q, %v; want 1.1.0", gh, v, err)
		}
		if tag != "1.1.0" {
			t.Errorf("%s: tag = %q; want 1.1.0", gh, tag)
		}
	}
}

func TestResolveConstraintAndIgnore(t *testing.T) {
	saveSeams(t)
	gitLsRemoteTags = func(string) ([]string, error) {
		return []string{"1.2.0", "1.5.0", "2.0.0"}, nil
	}
	// constraint filters out 2.0.0, so max satisfying ^1 is 1.5.0.
	if v, tag, err := Resolve(map[string]any{"github": "o/r"}, "^1"); err != nil || v != "1.5.0" || tag != "1.5.0" {
		t.Errorf("constraint: %q %q %v", v, tag, err)
	}
	// ignore drops the 1.x line, leaving 2.0.0.
	spec := map[string]any{"github": "o/r", "ignore": `/^1\./`}
	if v, tag, err := Resolve(spec, "*"); err != nil || v != "2.0.0" || tag != "2.0.0" {
		t.Errorf("ignore: %q %q %v", v, tag, err)
	}
}

func TestResolveUnparseableCandidatesSkipped(t *testing.T) {
	saveSeams(t)
	httpGet = func(string) (string, error) { return "foo 1.2.3 bar", nil }
	// match grabs words; "foo"/"bar" are unparseable and skipped, 1.2.3 wins.
	spec := map[string]any{"url": "u", "match": `/[a-z0-9.]+/`}
	if v, tag, err := Resolve(spec, "*"); err != nil || v != "1.2.3" || tag != "1.2.3" {
		t.Errorf("Resolve = %q %q %v; want 1.2.3", v, tag, err)
	}
}

func TestResolveErrors(t *testing.T) {
	saveSeams(t)
	cases := map[string]struct {
		spec       any
		constraint string
		setup      func()
	}{
		"not-a-map":      {spec: "hello"},
		"bad-strip":      {spec: map[string]any{"github": "o/r", "strip": "/[/"}},
		"bad-ignore":     {spec: map[string]any{"github": "o/r", "ignore": "/[/"}},
		"bad-github":     {spec: map[string]any{"github": "owner"}},
		"empty-github":   {spec: map[string]any{"github": ""}},
		"git-error":      {spec: map[string]any{"github": "o/r"}, setup: func() { gitLsRemoteTags = func(string) ([]string, error) { return nil, errBoom } }},
		"url-no-match":   {spec: map[string]any{"url": "u"}},
		"match-no-url":   {spec: map[string]any{"match": "/x/"}},
		"bad-match":      {spec: map[string]any{"url": "u", "match": "/[/"}},
		"http-error":     {spec: map[string]any{"url": "u", "match": "/x/"}, setup: func() { httpGet = func(string) (string, error) { return "", errBoom } }},
		"neither":        {spec: map[string]any{"summary": "x"}},
		"no-candidate":   {spec: map[string]any{"url": "u", "match": `/\d+\.\d+/`}, setup: func() { httpGet = func(string) (string, error) { return "no versions here", nil } }},
		"bad-constraint": {spec: map[string]any{"github": "o/r"}, constraint: "@@@bad", setup: func() { gitLsRemoteTags = func(string) ([]string, error) { return []string{"1.0.0"}, nil } }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			saveSeams(t)
			if tc.setup != nil {
				tc.setup()
			}
			if _, _, err := Resolve(tc.spec, tc.constraint); err == nil {
				t.Errorf("%s: expected error", name)
			}
		})
	}
}

func TestRegexList(t *testing.T) {
	if l, err := regexList(nil); err != nil || l != nil {
		t.Errorf("nil: %v %v", l, err)
	}
	if l, err := regexList("/x/"); err != nil || len(l) != 1 {
		t.Errorf("string: %v %v", l, err)
	}
	if l, err := regexList([]any{"/a/", "/b/"}); err != nil || len(l) != 2 {
		t.Errorf("list: %v %v", l, err)
	}
	if _, err := regexList([]any{5}); err == nil {
		t.Error("non-string list entry: want error")
	}
	if _, err := regexList([]any{"/[/"}); err == nil {
		t.Error("bad regex in list: want error")
	}
	if _, err := regexList(42); err == nil {
		t.Error("invalid type: want error")
	}
}

func TestCompileDelim(t *testing.T) {
	if re, err := compileDelim("/ab/"); err != nil || re.String() != "ab" {
		t.Errorf("delimited: %v %v", re, err)
	}
	if re, err := compileDelim("ab"); err != nil || re.String() != "ab" {
		t.Errorf("bare: %v %v", re, err)
	}
	if _, err := compileDelim("/[/"); err == nil {
		t.Error("bad regex: want error")
	}
}

func TestGithubRepoURL(t *testing.T) {
	if u, err := githubRepoURL("o/r/tags"); err != nil || u != "https://github.com/o/r" {
		t.Errorf("valid: %q %v", u, err)
	}
	if _, err := githubRepoURL("solo"); err == nil {
		t.Error("no repo: want error")
	}
	if _, err := githubRepoURL("/r"); err == nil {
		t.Error("empty owner: want error")
	}
}

func TestParseLsRemote(t *testing.T) {
	out := "sha1\trefs/tags/v1.0.0\n\nsha2\trefs/heads/main\nsha3\trefs/tags/v2.0.0\n"
	got := parseLsRemote(out)
	if len(got) != 2 || got[0] != "v1.0.0" || got[1] != "v2.0.0" {
		t.Errorf("parseLsRemote = %v", got)
	}
}

func TestHTTPGetSeam(t *testing.T) {
	saveSeams(t)
	// success
	httpGetRaw = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("hello"))}, nil
	}
	if body, err := httpGet("u"); err != nil || body != "hello" {
		t.Errorf("success: %q %v", body, err)
	}
	// GET error
	httpGetRaw = func(string) (*http.Response, error) { return nil, errBoom }
	if _, err := httpGet("u"); err == nil {
		t.Error("GET error: want error")
	}
	// non-200
	httpGetRaw = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	if _, err := httpGet("u"); err == nil {
		t.Error("404: want error")
	}
	// read error
	httpGetRaw = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("x"))}, nil
	}
	ioReadAll = func(io.Reader) ([]byte, error) { return nil, errBoom }
	if _, err := httpGet("u"); err == nil {
		t.Error("read error: want error")
	}
}

func TestGitLsRemoteTagsSeam(t *testing.T) {
	saveSeams(t)
	// success: fake git prints ls-remote-shaped output.
	execCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("printf", "sha1\\trefs/tags/v1.0.0\\nsha2\\trefs/tags/v2.0.0\\n")
	}
	tags, err := gitLsRemoteTags("https://github.com/o/r")
	if err != nil || len(tags) != 2 || tags[0] != "v1.0.0" || tags[1] != "v2.0.0" {
		t.Fatalf("tags = %v, err %v", tags, err)
	}
	// failure: non-zero exit.
	execCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "exit 3")
	}
	if _, err := gitLsRemoteTags("https://github.com/o/r"); err == nil {
		t.Error("git failure: want error")
	}
}
