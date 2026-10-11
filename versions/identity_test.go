package versions

import "testing"

// ⛔ THE SUFFIX FORMS ARE THE POINT. A `github:` spec also takes
// owner/repo/tags, owner/repo/releases and owner/repo/releases/tags, and a
// naive split on "/" keeps the suffix as part of the repository name — which
// would produce a key no database has ever heard of.
func TestIdentitiesReadsEveryGithubSpecForm(t *testing.T) {
	for _, spec := range []string{
		"errata-ai/vale",
		"errata-ai/vale/tags",
		"errata-ai/vale/releases",
		"errata-ai/vale/releases/tags",
	} {
		got := Identities(map[string]any{"github": spec})
		if len(got) != 1 {
			t.Fatalf("%q gave %+v", spec, got)
		}
		if got[0].DB != "GitHub" || got[0].Name != "errata-ai/vale" {
			t.Errorf("%q gave %+v, want GitHub errata-ai/vale", spec, got[0])
		}
	}
}

// ⛔ A LIST OF SOURCES NAMES SEVERAL IDENTITIES, and taking the first would
// silently drop the rest. Resolve walks both shapes; so must this.
func TestIdentitiesWalksAListOfSources(t *testing.T) {
	got := Identities([]any{
		map[string]any{"github": "a/one"},
		map[string]any{"gitlab": "b/two"},
		map[string]any{"npm": "left-pad"},
	})
	if len(got) != 3 {
		t.Fatalf("read %d identities, want 3: %+v", len(got), got)
	}
	// Sorted by database then name, so two runs agree.
	if got[0].DB != "GitHub" || got[1].DB != "GitLab" || got[2].DB != "npm" {
		t.Errorf("not sorted by database: %+v", got)
	}
	if got[2].Name != "left-pad" {
		t.Errorf("the npm key is wrong: %+v", got[2])
	}
}

// AN npm SPEC IS AN ECOSYSTEM KEY, not a repository — the better of the two,
// because OSV keys npm advisories by package name with curated ranges.
func TestAnNpmSpecIsAnEcosystemKey(t *testing.T) {
	got := Identities(map[string]any{"npm": "@scope/pkg"})
	if len(got) != 1 || got[0].DB != "npm" || got[0].Name != "@scope/pkg" {
		t.Errorf("got %+v", got)
	}
}

// A SPEC THAT NAMES NOTHING QUERYABLE RETURNS NOTHING, rather than a key made
// up from the host. `url:` is the 302-recipe case the census found: it offers
// an upstream host and no identity any database accepts, and inventing one
// from it is how a CPE guess becomes an assertion.
func TestASpecWithOnlyAUrlNamesNoIdentity(t *testing.T) {
	for name, spec := range map[string]any{
		"a url block":      map[string]any{"url": "https://ftp.gnu.org/gnu/bash/bash-5.3.tar.gz"},
		"a literal list":   []any{"5.3", "5.2"},
		"nothing at all":   nil,
		"an empty github":  map[string]any{"github": ""},
		"a malformed slug": map[string]any{"github": "no-slash"},
	} {
		if got := Identities(spec); len(got) != 0 {
			t.Errorf("%s produced %+v", name, got)
		}
	}
	// THE POSITIVE CONTROL: the same function does find a real one, so the
	// five above are not passing on an extractor that finds nothing.
	if got := Identities(map[string]any{"github": "a/b"}); len(got) != 1 {
		t.Fatal("no identity is found at all, so the negatives prove nothing")
	}
}

// DUPLICATES COLLAPSE. A recipe listing the same repo under two source blocks
// names one identity, and counting it twice would inflate every total built on
// this.
func TestIdentitiesDeduplicates(t *testing.T) {
	got := Identities([]any{
		map[string]any{"github": "a/b"},
		map[string]any{"github": "a/b/releases"},
	})
	if len(got) != 1 {
		t.Errorf("got %+v, want one identity", got)
	}
}

// ⛔ THE TIE-BREAK IS WHAT MAKES THE ORDER TOTAL, and no other test here has
// two identities sharing a database — so the name comparison was uncovered,
// and sort.Slice is NOT stable: without it two GitHub keys would come back in
// whichever order the map happened to yield. That is the same
// non-determinism the comment above it warns about, one level down.
func TestTwoIdentitiesInOneDatabaseAreOrderedByName(t *testing.T) {
	spec := []any{
		map[string]any{"github": "zeta/last"},
		map[string]any{"github": "alpha/first"},
	}
	// Run it more than once: a single pass can agree with the wanted order
	// by luck when the comparison is missing.
	for i := 0; i < 8; i++ {
		got := Identities(spec)
		if len(got) != 2 {
			t.Fatalf("read %d identities: %+v", len(got), got)
		}
		if got[0].Name != "alpha/first" || got[1].Name != "zeta/last" {
			t.Fatalf("pass %d ordered %+v", i, got)
		}
	}
}
