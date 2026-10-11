package vuln

import "testing"

// The four projects measured as having NO identity while their own names said
// both the ecosystem and the package.
func TestAProjectNamedAfterARegistryIsIdentified(t *testing.T) {
	for _, c := range []struct{ project, db, name string }{
		{"crates.io/bpb", "crates.io", "bpb"},
		{"crates.io/huniq", "crates.io", "huniq"},
		{"crates.io/albion_terminal_rpg", "crates.io", "albion_terminal_rpg"},
		// The host and the ecosystem are DIFFERENT WORDS here, which is the
		// mistake that produces a query matching nothing.
		{"npmjs.com/left-pad", "npm", "left-pad"},
		{"rubygems.org/rails", "RubyGems", "rails"},
		{"pypi.org/requests", "PyPI", "requests"},
	} {
		ids := FromProjectName(c.project)
		if len(ids) != 1 || ids[0].DB != c.db || ids[0].Name != c.name {
			t.Errorf("FromProjectName(%q) = %v, want %s:%s", c.project, ids, c.db, c.name)
		}
	}
}

// Deliberately narrow: it reads the pantry's naming convention and invents
// nothing. Each of these would be a guess.
func TestFromProjectNameInventsNothing(t *testing.T) {
	for _, project := range []string{
		"crates.io",       // the registry, no package
		"crates.io/",      // same, with a slash
		"crates.io/a/b",   // not a shape the convention produces
		"openssl.org",     // no slash at all
		"ftp.gnu.org/tar", // a source host, not a registry — the CPE case
		"github.com/o/r",  // a forge, and two segments
		"",
	} {
		if ids := FromProjectName(project); ids != nil {
			t.Errorf("FromProjectName(%q) = %v, want nothing", project, ids)
		}
	}
}

// ⛔ The two lists are edited separately, so they are compared here. CPAN and
// CocoaPods were in the first version of ecosystemHosts and are not OSV
// ecosystems: they would have produced a key no database answers, silently.
func TestEveryEcosystemHostNamesAKnownDatabase(t *testing.T) {
	for host, eco := range ecosystemHosts {
		if !knownDBs[eco] {
			t.Errorf("%s maps to %q, which the table would refuse", host, eco)
		}
	}
	if len(ecosystemHosts) == 0 {
		t.Fatal("the map is empty, so this test compared nothing")
	}
}

// Both derived sources are reported, not one of them: the project name and the
// versions block name two different databases, and asking both is the point.
func TestTheProjectNameAndTheRecipeAreBothReported(t *testing.T) {
	ids, src := (*Table)(nil).For("crates.io/bpb", map[string]any{"github": "withoutboats/bpb"})
	if src != SourceDerived {
		t.Fatalf("source = %v, want derived", src)
	}
	if len(ids) != 2 {
		t.Fatalf("identities = %v, want both the crate and the repo", ids)
	}
	// Sorted, so a census over the pantry is reproducible.
	if ids[0] != (Identity{DB: "GitHub", Name: "withoutboats/bpb"}) || ids[1] != (Identity{DB: "crates.io", Name: "bpb"}) {
		t.Errorf("identities = %v", ids)
	}
}

// A declared entry still wins over both.
func TestDeclaredBeatsTheProjectNameToo(t *testing.T) {
	tab, err := ParseTable([]byte("schema-version: 1\nprojects:\n  crates.io/bpb:\n    cpe: [withoutboats:bpb]\n"))
	if err != nil {
		t.Fatalf("ParseTable: %v", err)
	}
	ids, src := tab.For("crates.io/bpb", map[string]any{"github": "withoutboats/bpb"})
	if src != SourceDeclared || len(ids) != 1 || ids[0].DB != "cpe" {
		t.Errorf("identities = %v, source = %v", ids, src)
	}
}

// The same key from both sources is reported ONCE. A duplicate would be
// counted twice in every census built on this.
func TestOneKeyFromTwoSourcesIsReportedOnce(t *testing.T) {
	ids, _ := (*Table)(nil).For("npmjs.com/left-pad", map[string]any{"npm": "left-pad"})
	if len(ids) != 1 {
		t.Errorf("identities = %v, want one", ids)
	}
}

// And a project whose name says nothing still derives from its recipe alone.
func TestARecipeOnlyProjectIsUnaffected(t *testing.T) {
	ids, src := (*Table)(nil).For("curl.se", map[string]any{"github": "curl/curl"})
	if src != SourceDerived || len(ids) != 1 || ids[0].DB != "GitHub" {
		t.Errorf("identities = %v, source = %v", ids, src)
	}
}
