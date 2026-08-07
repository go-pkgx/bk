package versions

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errBoom = errors.New("boom")

// saveSeams snapshots every injectable seam and restores it after the test.
func saveSeams(t *testing.T) {
	t.Helper()
	hg, glt, hgr, ira := httpGet, gitLsRemoteTags, httpGetRaw, ioReadAll
	t.Cleanup(func() {
		httpGet, gitLsRemoteTags, httpGetRaw, ioReadAll = hg, glt, hgr, ira
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

func TestResolveNpm(t *testing.T) {
	saveSeams(t)
	var gotURL string
	httpGet = func(u string) (string, error) {
		gotURL = u
		return `{"name":"prettier","dist-tags":{"latest":"3.9.6"},"versions":{"3.9.5":{},"3.9.6":{},"2.0.0":{}}}`, nil
	}
	v, _, err := Resolve(map[string]any{"npm": "prettier"}, "*")
	if err != nil || v != "3.9.6" {
		t.Fatalf("npm resolve = %q %v; want 3.9.6", v, err)
	}
	if gotURL != "https://registry.npmjs.org/prettier" {
		t.Errorf("unscoped URL = %q", gotURL)
	}
	// scoped name → slash percent-encoded
	if got := npmRegistryURL("@angular/cli"); got != "https://registry.npmjs.org/@angular%2Fcli" {
		t.Errorf("scoped URL = %q", got)
	}
	// a constrained pick over the registry versions
	if v, _, err := Resolve(map[string]any{"npm": "prettier"}, "^2"); err != nil || v != "2.0.0" {
		t.Errorf("npm ^2 = %q %v", v, err)
	}
	// malformed registry JSON → error
	httpGet = func(string) (string, error) { return "not json", nil }
	if _, _, err := Resolve(map[string]any{"npm": "x"}, "*"); err == nil {
		t.Error("expected npm JSON parse error")
	}
	// httpGet failure propagates
	httpGet = func(string) (string, error) { return "", errBoom }
	if _, _, err := Resolve(map[string]any{"npm": "x"}, "*"); err == nil {
		t.Error("expected npm fetch error")
	}
}

// TestResolveURLCalVer covers the date-style CalVer path (curl.se/ca-certs):
// dash-separated all-numeric versions are rewritten to dotted form so they
// parse and the highest date wins, with zero-padding preserved in version.raw.
func TestResolveURLCalVer(t *testing.T) {
	saveSeams(t)
	listing := `cacert-2025-05-20.pem cacert-2025-08-12.pem cacert-2026-03-19.pem`
	httpGet = func(string) (string, error) { return listing, nil }
	spec := map[string]any{
		"url":   "https://curl.se/docs/caextract.html",
		"match": `/cacert-2\d+-\d+-\d+.pem/`,
		"strip": []any{"/cacert-/", "/\\.pem/"},
	}
	v, tag, err := Resolve(spec, "*")
	if err != nil || v != "2026.03.19" {
		t.Fatalf("Resolve = %q, %v; want 2026.03.19 (padded, dotted)", v, err)
	}
	if tag != "cacert-2026-03-19.pem" {
		t.Fatalf("tag = %q; want cacert-2026-03-19.pem", tag)
	}
	// direct unit check of both branches
	if calverToDotted("2025-08-12") != "2025.08.12" || calverToDotted("1.2.3") != "1.2.3" {
		t.Error("calverToDotted branches")
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

func TestResolveGitlab(t *testing.T) {
	saveSeams(t)
	var gotURL string
	gitLsRemoteTags = func(repoURL string) ([]string, error) {
		gotURL = repoURL
		return []string{"v2.1.0", "v2.3.0", "v2.2.0"}, nil
	}
	// default host + decorative /tags suffix
	v, tag, err := Resolve(map[string]any{"gitlab": "group/project/tags", "strip": "/v/"}, "")
	if err != nil || v != "2.3.0" || tag != "v2.3.0" {
		t.Fatalf("gitlab default = %q/%q %v", v, tag, err)
	}
	if gotURL != "https://gitlab.com/group/project" {
		t.Errorf("default-host URL = %q", gotURL)
	}
	// self-hosted "host:" prefix + nested subgroup path
	if _, _, err := Resolve(map[string]any{"gitlab": "gitlab.gnome.org:GNOME/pango/tags"}, "*"); err != nil {
		t.Fatalf("gitlab custom host: %v", err)
	}
	if gotURL != "https://gitlab.gnome.org/GNOME/pango" {
		t.Errorf("custom-host URL = %q", gotURL)
	}
	// invalid spec (no group/project) → error, no ls-remote
	if _, _, err := Resolve(map[string]any{"gitlab": "loneword"}, "*"); err == nil {
		t.Error("expected invalid-gitlab-spec error")
	}
	// a failing ls-remote propagates
	gitLsRemoteTags = func(string) ([]string, error) { return nil, errBoom }
	if _, _, err := Resolve(map[string]any{"gitlab": "group/project"}, "*"); err == nil {
		t.Error("expected gitlab ls-remote error to propagate")
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

func TestResolveListForm(t *testing.T) {
	saveSeams(t)
	// A single-entry hardcoded list: the entry is both the version and the
	// tag, and its raw scalar text ("3.0") must survive verbatim — NOT be
	// re-rendered as "3" (which would 404 the distributable URL).
	if v, tag, err := Resolve([]any{"3.0"}, "*"); err != nil || v != "3.0" || tag != "3.0" {
		t.Errorf("single-entry list = %q %q %v; want 3.0/3.0", v, tag, err)
	}
	// Multi-entry list with no constraint selects the max.
	if v, tag, err := Resolve([]any{"3.0", "3.1", "7", "7.0.6"}, ""); err != nil || v != "7.0.6" || tag != "7.0.6" {
		t.Errorf("multi-entry max = %q %q %v; want 7.0.6", v, tag, err)
	}
	// A constraint picks the max satisfying it, not the overall max.
	if v, _, err := Resolve([]any{"3.0", "3.1", "7", "7.0.6"}, "^3"); err != nil || v != "3.1" {
		t.Errorf("constrained list = %q %v; want 3.1", v, err)
	}
	// An int/string/float mix (as pantry hands it over: verbatim scalar text)
	// preserves each entry's exact text.
	if v, tag, err := Resolve([]any{"7", "3.0"}, "^3"); err != nil || v != "3.0" || tag != "3.0" {
		t.Errorf("mixed-text list = %q %q %v; want 3.0/3.0", v, tag, err)
	}
	// An empty list has no candidates.
	if _, _, err := Resolve([]any{}, "*"); err == nil {
		t.Error("empty list: expected no-candidate error")
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

// gitAdvertisement builds a git smart-HTTP upload-pack ref advertisement
// (the GET /info/refs?service=git-upload-pack body) from ordered name→sha refs;
// the first ref carries the \x00-delimited capabilities, per the protocol.
func gitAdvertisement(refs [][2]string) string {
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	var b strings.Builder
	b.WriteString(pkt("# service=git-upload-pack\n") + "0000")
	for i, r := range refs {
		line := r[1] + " " + r[0]
		if i == 0 {
			line += "\x00symref=HEAD:refs/heads/main agent=test"
		}
		b.WriteString(pkt(line + "\n"))
	}
	b.WriteString("0000")
	return b.String()
}

func TestGitLsRemoteTagsSeam(t *testing.T) {
	saveSeams(t)
	// Pure-Go go-git ls-remote against a smart-HTTP server advertising tags.
	adv := gitAdvertisement([][2]string{
		{"HEAD", "1111111111111111111111111111111111111111"},
		{"refs/heads/main", "1111111111111111111111111111111111111111"},
		{"refs/tags/v1.0.0", "2222222222222222222222222222222222222222"},
		{"refs/tags/v2.0.0", "3333333333333333333333333333333333333333"},
		{"refs/tags/v2.0.0^{}", "4444444444444444444444444444444444444444"}, // peeled → dropped
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, adv)
	}))
	defer srv.Close()
	tags, err := gitLsRemoteTags(srv.URL)
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	got := map[string]bool{}
	for _, tg := range tags {
		got[tg] = true
	}
	if len(tags) != 2 || !got["v1.0.0"] || !got["v2.0.0"] {
		t.Fatalf("tags = %v (want v1.0.0,v2.0.0; HEAD/branch excluded, ^{} dropped)", tags)
	}
	// a server that does not speak the protocol → error
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer bad.Close()
	if _, err := gitLsRemoteTags(bad.URL); err == nil {
		t.Error("bad server: want error")
	}
}
