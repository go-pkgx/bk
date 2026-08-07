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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/storage/memory"
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

	// gitLsRemoteTags lists a repo's tag names with go-git's pure-Go remote
	// listing — NOT a shell-out to the `git` binary (keeps bk portable /
	// dependency-free) and NOT the GitHub API (which rate-limits unauthenticated
	// CI at 60 req/h). go-git speaks the smart-HTTP ref advertisement in-process
	// and works for any Git host (GitHub and GitLab alike). Short() strips the
	// leading refs/tags/; peeled "^{}" entries are skipped.
	gitLsRemoteTags = func(repoURL string) ([]string, error) {
		rem := gogit.NewRemote(memory.NewStorage(), &gitconfig.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
		refs, err := rem.List(&gogit.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("versions: ls-remote %s: %w", repoURL, err)
		}
		var tags []string
		for _, ref := range refs {
			if ref.Name().IsTag() && !strings.HasSuffix(ref.Name().String(), "^{}") {
				tags = append(tags, ref.Name().Short())
			}
		}
		return tags, nil
	}

	// Low-level stdlib references (no body of their own to cover here).
	httpGetRaw = http.Get
	ioReadAll  = io.ReadAll
)

// Resolve returns both the version string (as it appears upstream, for
// {{version.raw}}) and the raw upstream git tag it was resolved from (for
// {{version.tag}}) selected from a recipe's `versions:` spec, honouring an
// optional constraint ("" or "*" = latest). spec is pantry.Recipe.Versions,
// an `any` unmarshalled from YAML.
//
// The returned tag is the pre-strip candidate that produced the winning
// version: for the github path it is the `refs/tags/`-trimmed tag (e.g.
// "v1.2.3" or "openssl-3.5.0"); for the url/match path it is the matched
// string before any strip. Recipes interpolate it in GitHub release URLs
// like `.../releases/download/{{version.tag}}/{{version.tag}}.tar.gz`.
//
// A list-form `versions:` (an explicit, hardcoded set of candidate versions)
// decodes as a []any of the entries' verbatim scalar text (pantry preserves
// that text so e.g. "3.0" is not coerced to "3"). Those entries ARE the
// candidates: there is no upstream github/url resolution, no strip/ignore, and
// the winning candidate is its own {{version.tag}}.
func Resolve(spec any, constraint string) (string, string, error) {
	switch v := spec.(type) {
	case map[string]any:
		return resolveMap(v, constraint)
	case []any:
		candidates := make([]string, 0, len(v))
		for _, e := range v {
			candidates = append(candidates, fmt.Sprint(e))
		}
		return selectVersion(candidates, nil, nil, constraint)
	default:
		return "", "", fmt.Errorf("versions: unsupported version spec %T", spec)
	}
}

// resolveMap handles the github/url map form of `versions:`, listing upstream
// tags (or matching an upstream listing) and selecting via the loose semver.
func resolveMap(m map[string]any, constraint string) (string, string, error) {
	strips, err := regexList(m["strip"])
	if err != nil {
		return "", "", err
	}
	ignores, err := regexList(m["ignore"])
	if err != nil {
		return "", "", err
	}

	var candidates []string
	switch {
	case m["github"] != nil:
		gh, _ := m["github"].(string)
		repoURL, err := githubRepoURL(gh)
		if err != nil {
			return "", "", err
		}
		tags, err := gitLsRemoteTags(repoURL)
		if err != nil {
			return "", "", err
		}
		for _, t := range tags {
			candidates = append(candidates, strings.TrimPrefix(t, "refs/tags/"))
		}
	case m["gitlab"] != nil:
		gl, _ := m["gitlab"].(string)
		repoURL, err := gitlabRepoURL(gl)
		if err != nil {
			return "", "", err
		}
		tags, err := gitLsRemoteTags(repoURL)
		if err != nil {
			return "", "", err
		}
		for _, t := range tags {
			candidates = append(candidates, strings.TrimPrefix(t, "refs/tags/"))
		}
	case m["npm"] != nil:
		pkg, _ := m["npm"].(string)
		body, err := httpGet(npmRegistryURL(pkg))
		if err != nil {
			return "", "", err
		}
		var doc struct {
			Versions map[string]json.RawMessage `json:"versions"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			return "", "", fmt.Errorf("versions: npm %s: %w", pkg, err)
		}
		for v := range doc.Versions {
			candidates = append(candidates, v)
		}
	case m["url"] != nil:
		u, _ := m["url"].(string)
		matchRaw, _ := m["match"].(string)
		if u == "" || matchRaw == "" {
			return "", "", fmt.Errorf("versions: url spec needs both url and match")
		}
		re, err := compileDelim(matchRaw)
		if err != nil {
			return "", "", err
		}
		body, err := httpGet(u)
		if err != nil {
			return "", "", err
		}
		candidates = re.FindAllString(body, -1)
	default:
		return "", "", fmt.Errorf("versions: spec has neither github nor url")
	}

	return selectVersion(candidates, strips, ignores, constraint)
}

// selectVersion applies strip (remove) then ignore (discard) to each
// candidate, parses the survivors with the loose semver, and returns both the
// (post-strip, v-normalised) version string of the max version satisfying
// constraint and the raw pre-strip candidate that produced it (the upstream
// git tag, for {{version.tag}}).
// calverDashRE matches an all-numeric, dash-separated version — a date-style
// CalVer such as 2025-08-12 (or 2024-1) — with no other characters.
var calverDashRE = regexp.MustCompile(`^\d+(-\d+)+$`)

// calverToDotted rewrites such a CalVer to dotted form (2025-08-12 → 2025.08.12)
// so the loose semver parses it; semver otherwise reads the first '-' as the
// start of a pre-release and the version fails to parse. Zero-padding is kept so
// {{version.raw}} round-trips (recipes map dots↔dashes, e.g. ca-certs does
// `tr . -` to rebuild cacert-YYYY-MM-DD.pem). Non-CalVer strings are unchanged,
// so this can only rescue candidates that would otherwise be skipped.
func calverToDotted(s string) string {
	if calverDashRE.MatchString(s) {
		return strings.ReplaceAll(s, "-", ".")
	}
	return s
}

// npmRegistryURL builds the npm registry document URL for a package. A scoped
// name (@scope/name) must have its slash percent-encoded (@scope%2Fname); an
// unscoped name is used as-is.
func npmRegistryURL(pkg string) string {
	if strings.HasPrefix(pkg, "@") {
		pkg = strings.Replace(pkg, "/", "%2F", 1)
	}
	return "https://registry.npmjs.org/" + pkg
}

func selectVersion(candidates []string, strips, ignores []*regexp.Regexp, constraint string) (string, string, error) {
	var rng *semver.Range
	if constraint != "" && constraint != "*" {
		r, err := semver.ParseRange(constraint)
		if err != nil {
			return "", "", fmt.Errorf("versions: bad constraint %q: %w", constraint, err)
		}
		rng = r
	}

	var best *semver.Version
	var bestStr, bestTag string
	for _, c := range candidates {
		s := c
		for _, re := range strips {
			s = re.ReplaceAllString(s, "")
		}
		s = calverToDotted(s)
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
			best, bestStr, bestTag = v, s, c
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("versions: no candidate version matched")
	}
	return dropVPrefix(bestStr), bestTag, nil
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

// gitlabRepoURL turns a pantry `gitlab:` spec into a clone URL. The spec is
// "[host:]group/project[/…][/tags]": an optional "host:" prefix selects a
// self-hosted GitLab (default gitlab.com), a trailing "/tags" is decorative, and
// the remaining path — which may be nested through subgroups — is the project.
// GitLab serves tags over the same `git ls-remote` as GitHub, so only the URL
// differs.
func gitlabRepoURL(gl string) (string, error) {
	host, path := "gitlab.com", gl
	if i := strings.IndexByte(gl, ':'); i >= 0 {
		host, path = gl[:i], gl[i+1:]
	}
	path = strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), "/tags"), "/")
	if host == "" || !strings.Contains(path, "/") {
		return "", fmt.Errorf("versions: invalid gitlab spec %q", gl)
	}
	return "https://" + host + "/" + path, nil
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

