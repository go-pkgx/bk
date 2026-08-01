// Package moustache implements pkgx's `{{token}}` template substitution, a
// faithful port of libpkgx's useMoustaches. Build and test scripts, env values
// and fixtures are expanded with it before they run.
package moustache

import (
	"regexp"
	"strconv"
	"strings"
)

// Token is a single substitution: every `{{ From }}` becomes To.
type Token struct{ From, To string }

// Apply substitutes every token in input. The pattern mirrors libpkgx exactly:
// `(^\$)?{{\s*<from>\s*}}` — optional surrounding whitespace, and a single
// leading `$` (at the very start) is consumed so `${{prefix}}` yields the value
// without the dollar. Replacement is literal (a To containing `$` is safe).
func Apply(input string, tokens []Token) string {
	for _, t := range tokens {
		re := regexp.MustCompile(`(^\$)?\{\{\s*` + regexp.QuoteMeta(t.From) + `\s*\}\}`)
		input = re.ReplaceAllStringFunc(input, func(string) string { return t.To })
	}
	return input
}

// Version returns the version tokens for a version string under prefix
// (usually "version", or "deps.<project>.version" for a dependency).
func Version(version, prefix string) []Token {
	raw := strings.TrimSpace(version)
	core := strings.TrimPrefix(raw, "v")
	build := ""
	if i := strings.IndexByte(core, "+"[0]); i >= 0 {
		build = core[i+1:]
		core = core[:i]
	}
	// tag = a trailing pre-release after '-' (pkgx keeps it as version.tag)
	tag := ""
	if i := strings.IndexByte(core, '-'); i >= 0 {
		tag = core[i+1:]
		core = core[:i]
	}
	nums := numericParts(core)
	major, minor, patch := nth(nums, 0), nth(nums, 1), nth(nums, 2)

	toks := []Token{
		{prefix, itoa(major) + "." + itoa(minor) + "." + itoa(patch)},
		{prefix + ".major", itoa(major)},
		{prefix + ".minor", itoa(minor)},
		{prefix + ".patch", itoa(patch)},
		{prefix + ".marketing", itoa(major) + "." + itoa(minor)},
		{prefix + ".build", build},
		{prefix + ".raw", core},
	}
	if tag != "" {
		toks = append(toks, Token{prefix + ".tag", tag})
	}
	return toks
}

// Dep is an installed dependency contributing deps.<Project>.* tokens.
type Dep struct {
	Project string
	Version string
	Path    string
}

// Deps returns the tokens for a build's resolved dependencies.
func Deps(deps []Dep) []Token {
	var toks []Token
	for _, d := range deps {
		toks = append(toks, Token{"deps." + d.Project + ".prefix", d.Path})
		toks = append(toks, Version(d.Version, "deps."+d.Project+".version")...)
	}
	return toks
}

// Prefix returns the {{prefix}} token (the package's install path).
func Prefix(installPath string) []Token { return []Token{{"prefix", installPath}} }

// Host returns the {{hw.*}} tokens describing the build TARGET.
func Host(arch, triple, platform string, concurrency int) []Token {
	return []Token{
		{"hw.arch", arch},
		{"hw.target", triple},
		{"hw.platform", platform},
		{"hw.concurrency", itoa(concurrency)},
	}
}

func numericParts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ".") {
		n, i := 0, 0
		for i < len(p) && p[i] >= '0' && p[i] <= '9' {
			n = n*10 + int(p[i]-'0')
			i++
		}
		out = append(out, n)
	}
	return out
}

func nth(xs []int, i int) int {
	if i < len(xs) {
		return xs[i]
	}
	return 0
}

func itoa(i int) string { return strconv.Itoa(i) }
