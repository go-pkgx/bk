package vuln

import "strings"

// The project NAME is a third source of identity, and for 114 projects it is a
// better one than the recipe.
//
// Measured 2026-10-11 on a 1563-recipe pantry: `crates.io/bpb`,
// `crates.io/huniq`, `crates.io/stego` and `crates.io/albion_terminal_rpg`
// were reported as having NO identity at all, while their own names say both
// the ecosystem and the package. That is four projects the derivation could
// answer for and did not.
//
// The larger half is the 110 `crates.io/*` projects that DO derive something —
// a GitHub repository, from their `versions:` block. For those this adds a
// second, better key rather than a first one: OSV keys Rust advisories
// (RUSTSEC) by `crates.io` with curated version ranges, where a
// repository-keyed advisory has whatever range its filer wrote. Both are
// emitted, because two keys mean two databases can be asked and neither has to
// be guessed.
//
// This is NOT the same trick as reading a `url:`. A distributable URL says
// where the SOURCE came from, which is a host and not a package name — that is
// the 322-project CPE case, and inventing a package name from it is what
// produced 27 openssl CVEs including four from 2009. A project named
// `crates.io/bpb` is different in kind: the pantry's own naming convention put
// the registry and the package name there, and the package is what the
// registry calls it.

// ecosystemHosts maps the pantry's naming convention for a package registry
// onto the OSV ecosystem that registry's advisories are filed under.
//
// Keys are the host as the pantry spells it; values are OSV's ecosystem
// spelling, which is not always the same word — `npmjs.com` is the host and
// `npm` is the ecosystem. Getting that backwards produces a query that matches
// nothing, silently.
// Three of these seven are present in the pantry today — crates.io (114
// projects), npmjs.com (1), rubygems.org (1). The other four are the registries
// pkgx names the same way and are listed because an absent entry is a project
// silently without an identity, which is the failure this file exists to fix;
// a map entry that is never read costs nothing.
//
// ⛔ Every value MUST be a key the table would accept. CPAN and CocoaPods were
// in the first version of this map and are NOT OSV ecosystems, so they would
// have produced a key no database answers — silently, because nothing compared
// the two lists. TestEveryEcosystemHostNamesAKnownDatabase is that comparison.
var ecosystemHosts = map[string]string{
	"crates.io":     "crates.io",
	"npmjs.com":     "npm",
	"pypi.org":      "PyPI",
	"rubygems.org":  "RubyGems",
	"packagist.org": "Packagist",
	"hex.pm":        "Hex",
	"nuget.org":     "NuGet",
}

// FromProjectName returns the identity a project's own name states, if it
// states one.
//
// It is deliberately narrow: exactly one path segment after a known registry
// host. `crates.io/bpb` yields a key; `crates.io` alone names the registry and
// no package; `crates.io/a/b` is not a shape the convention produces, and
// guessing which half is the package would be inventing.
func FromProjectName(project string) []Identity {
	host, rest, ok := strings.Cut(project, "/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return nil
	}
	eco, ok := ecosystemHosts[host]
	if !ok {
		return nil
	}
	return []Identity{{DB: eco, Name: rest}}
}
