// Package versions resolves a project's upstream version from a pantry
// recipe's `versions:` specification.
//
// This is deliberately distinct from bottle.PickVersion (which lists pkgx's
// already-built bottles at dist.pkgx.dev): dist may advertise a normalised
// version — e.g. ncurses 6.6.0 — that does not exist at the recipe's
// distributable URL (upstream only ships ncurses-6.5.tar.gz). For a fresh
// build the version MUST come from the recipe's own `versions:` field so it
// matches the distributable URL, which interpolates {{version.raw}}.
//
// The logic mirrors libpkgx's usePantry version handling: either list a
// GitHub repo's git tags, or GET an upstream listing and regex-match version
// strings, then strip/ignore/select via the loose pkgx-compatible semver.
package versions

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"

	"github.com/go-versions/semver"
)

// Injectable seams over the two external effects (network + git). Production
// uses the real implementations verbatim; tests swap them to feed fixture
// listings / tag lists and to exercise every error branch without a network.
var (
	// httpGet performs a GET and returns the response body as a string.
	httpGet = func(url string) (string, error) {
		resp, err := httpGetRaw(url)
		if err != nil {
			return "", fmt.Errorf("versions: GET %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("versions: GET %s: %s", url, resp.Status)
		}
		body, err := ioReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("versions: read %s: %w", url, err)
		}
		return string(body), nil
	}

	// gitLsRemoteTags lists a repo's tag names via
	// `git ls-remote --tags --refs <repoURL>` (not the GitHub API, which
	// rate-limits unauthenticated CI at 60 req/h).
	gitLsRemoteTags = func(repoURL string) ([]string, error) {
		out, err := execCommand("git", "ls-remote", "--tags", "--refs", repoURL).Output()
		if err != nil {
			return nil, fmt.Errorf("versions: git ls-remote %s: %w", repoURL, err)
		}
		return parseLsRemote(string(out)), nil
	}

	// Low-level stdlib references (no body of their own to cover here).
	httpGetRaw  = http.Get
	ioReadAll   = io.ReadAll
	execCommand = exec.Command
)

// Resolve returns the version string (as it appears upstream, for
// {{version.raw}}) selected from a recipe's `versions:` spec, honouring an
// optional constraint ("" or "*" = latest). spec is pantry.Recipe.Versions,
// an `any` unmarshalled from YAML.
func Resolve(spec any, constraint string) (string, error) {
	m, ok := spec.(map[string]any)
	if !ok {
		return "", fmt.Errorf("versions: unsupported version spec %T", spec)
	}

	strips, err := regexList(m["strip"])
	if err != nil {
		return "", err
	}
	ignores, err := regexList(m["ignore"])
	if err != nil {
		return "", err
	}

	var candidates []string
	switch {
	case m["github"] != nil:
		gh, _ := m["github"].(string)
		repoURL, err := githubRepoURL(gh)
		if err != nil {
			return "", err
		}
		tags, err := gitLsRemoteTags(repoURL)
		if err != nil {
			return "", err
		}
		for _, t := range tags {
			candidates = append(candidates, strings.TrimPrefix(t, "refs/tags/"))
		}
	case m["url"] != nil:
		u, _ := m["url"].(string)
		matchRaw, _ := m["match"].(string)
		if u == "" || matchRaw == "" {
			return "", fmt.Errorf("versions: url spec needs both url and match")
		}
		re, err := compileDelim(matchRaw)
		if err != nil {
			return "", err
		}
		body, err := httpGet(u)
		if err != nil {
			return "", err
		}
		candidates = re.FindAllString(body, -1)
	default:
		return "", fmt.Errorf("versions: spec has neither github nor url")
	}

	return selectVersion(candidates, strips, ignores, constraint)
}

// selectVersion applies strip (remove) then ignore (discard) to each
// candidate, parses the survivors with the loose semver, and returns the
// original (post-strip) string of the max version satisfying constraint.
func selectVersion(candidates []string, strips, ignores []*regexp.Regexp, constraint string) (string, error) {
	var rng *semver.Range
	if constraint != "" && constraint != "*" {
		r, err := semver.ParseRange(constraint)
		if err != nil {
			return "", fmt.Errorf("versions: bad constraint %q: %w", constraint, err)
		}
		rng = r
	}

	var best *semver.Version
	var bestStr string
	for _, c := range candidates {
		s := c
		for _, re := range strips {
			s = re.ReplaceAllString(s, "")
		}
		if matchesAny(ignores, s) {
			continue
		}
		v, err := semver.ParseVersion(s)
		if err != nil {
			continue
		}
		if rng != nil && !rng.Satisfies(v) {
			continue
		}
		if best == nil || v.Compare(best) > 0 {
			best, bestStr = v, s
		}
	}
	if best == nil {
		return "", fmt.Errorf("versions: no candidate version matched")
	}
	return dropVPrefix(bestStr), nil
}

// dropVPrefix strips a leading "v" before a digit ("v5.8.3" -> "5.8.3"), matching
// pkgx's tag normalisation (dist bottles are tagged without the v). Recipes whose
// `strip:` already removes the v are unaffected; this only catches github-tag
// specs that don't.
func dropVPrefix(s string) string {
	if len(s) > 1 && s[0] == 'v' && s[1] >= '0' && s[1] <= '9' {
		return s[1:]
	}
	return s
}

// githubRepoURL derives https://github.com/<owner>/<repo> from a `github:`
// value, accepting the bare owner/repo form and the owner/repo/tags,
// owner/repo/releases and owner/repo/releases/tags suffixes (all mean "list
// git tags").
func githubRepoURL(gh string) (string, error) {
	parts := strings.Split(gh, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("versions: invalid github spec %q", gh)
	}
	return "https://github.com/" + parts[0] + "/" + parts[1], nil
}

// regexList compiles a strip/ignore value, which may be absent, a single
// /regex/ (or bare) string, or a list of them.
func regexList(v any) ([]*regexp.Regexp, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		re, err := compileDelim(t)
		if err != nil {
			return nil, err
		}
		return []*regexp.Regexp{re}, nil
	case []any:
		out := make([]*regexp.Regexp, 0, len(t))
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("versions: strip/ignore entry not a string: %T", x)
			}
			re, err := compileDelim(s)
			if err != nil {
				return nil, err
			}
			out = append(out, re)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("versions: invalid strip/ignore %T", v)
	}
}

// compileDelim compiles s as a Go regexp, stripping surrounding /.../ slashes
// (pkgx's regex delimiters) first.
func compileDelim(s string) (*regexp.Regexp, error) {
	p := s
	if len(p) >= 2 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/") {
		p = p[1 : len(p)-1]
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("versions: bad regex %q: %w", s, err)
	}
	return re, nil
}

// matchesAny reports whether s matches any of the regexes.
func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// parseLsRemote extracts tag names from `git ls-remote --tags --refs` output,
// whose lines look like "<sha>\trefs/tags/<tag>".
func parseLsRemote(out string) []string {
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.Index(line, "refs/tags/"); i >= 0 {
			tags = append(tags, line[i+len("refs/tags/"):])
		}
	}
	return tags
}
