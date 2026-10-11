package versions

import "sort"

// Identity is a name a vulnerability database can be queried with.
//
// ⛔ THE PROBLEM IS THE IDENTITY, NOT THE DATA SOURCE. Measured 2026-10-11:
// `pkg:pkgx/openssl.org@4.0.2` is known to NO database — OSV answers `{}` to a
// `pkg:generic/...` purl — while NVD's CPE route answers for anything and
// answers badly: `cpeName=…openssl:3.5.7` returned 27 CVEs including four from
// 2009, because those records say `openssl:*` with no upper bound at all.
//
// A recipe already names something better. Of 1908 recipes, 1005 imply a
// package ecosystem and 1506 name a source repository; only 302 offer nothing
// but an upstream host, and those are concentrated on a handful (ftp.gnu.org
// 48, www.x.org 38, sourceforge 29). This turns what a recipe says into the
// key a database will accept.
type Identity struct {
	// DB is which database can be asked: "GitHub" and "GitLab" for advisories
	// keyed by repository, "npm" for the package ecosystem.
	DB string
	// Name is the key itself: owner/repo, or the package name.
	Name string
}

// Identities returns the keys a `versions:` spec names, sorted and without
// duplicates.
//
// It walks the same two shapes Resolve does — one source block, or a list of
// them — because a recipe with several sources names several identities and
// taking the first would silently drop the others.
//
// It makes NO network call and asserts nothing about whether the key exists
// upstream: it reports what the recipe claims, which is the half a database
// query needs before it can be made at all.
func Identities(spec any) []Identity {
	seen := map[Identity]bool{}
	collect(spec, seen)
	out := make([]Identity, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	// Sorted, so two runs over the same recipe report the same thing. A census
	// built on map order is not a census: ranging over a map gave this fleet
	// "Go 420" and then "Go 419" minutes apart.
	sort.Slice(out, func(i, j int) bool {
		if out[i].DB != out[j].DB {
			return out[i].DB < out[j].DB
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func collect(spec any, seen map[Identity]bool) {
	switch v := spec.(type) {
	case map[string]any:
		fromBlock(v, seen)
	case []any:
		for _, e := range v {
			collect(e, seen)
		}
	}
}

func fromBlock(m map[string]any, seen map[Identity]bool) {
	if gh, ok := m["github"].(string); ok && gh != "" {
		// githubOwnerRepo, not a split on "/": the spec also takes the
		// owner/repo/tags, owner/repo/releases and owner/repo/releases/tags
		// forms, and a naive split keeps the suffix as part of the repo.
		if owner, repo, err := githubOwnerRepo(gh); err == nil {
			seen[Identity{DB: "GitHub", Name: owner + "/" + repo}] = true
		}
	}
	if gl, ok := m["gitlab"].(string); ok && gl != "" {
		if owner, repo, err := githubOwnerRepo(gl); err == nil {
			seen[Identity{DB: "GitLab", Name: owner + "/" + repo}] = true
		}
	}
	// An npm spec is not a repository but an ECOSYSTEM key, which is the
	// better of the two: OSV keys npm advisories by package name and carries
	// curated version ranges for them.
	if n, ok := m["npm"].(string); ok && n != "" {
		seen[Identity{DB: "npm", Name: n}] = true
	}
}
