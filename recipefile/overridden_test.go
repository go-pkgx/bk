package recipefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/logical"
)

func writeRecipe(t *testing.T, pantry, project, name, body string) {
	t.Helper()
	dir := filepath.Join(pantry, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setFrom(t *testing.T, body string) *logical.Set {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "o.hcl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := logical.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const minimalYAML = `distributable:
  url: https://acme.org/{{version}}.tar.gz
dependencies:
  openssl.org: ^1.1
build:
  script: make install
provides:
  - bin/acme
`

func TestLoadOverriddenAppliesAndLeavesTheFileAlone(t *testing.T) {
	pantry := t.TempDir()
	writeRecipe(t, pantry, "acme.org", "package.yml", "# a comment nobody may lose\n"+minimalYAML)
	set := setFrom(t, `
project = "acme.org"
why     = "our registry carries no openssl 1.x"
edits   = [{ path = "dependencies[\"openssl.org\"]", set = "^3" }]
`)
	r, err := LoadOverridden(set, pantry, "acme.org")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Dependencies["openssl.org"]; got != "^3" {
		t.Errorf("dependency = %v, want ^3", got)
	}
	// Nothing on disk moved — which is the whole reason for applying at load:
	// rewriting the document would drop the comment.
	b, err := os.ReadFile(filepath.Join(pantry, "projects", "acme.org", "package.yml"))
	if err != nil || !strings.HasPrefix(string(b), "# a comment nobody may lose") {
		t.Errorf("the file was rewritten: %q", string(b))
	}
}

// A project the set does not mention takes exactly the path it always took.
// Most of the pantry is in that position during the migration.
func TestLoadOverriddenLeavesAnUnmentionedProjectAlone(t *testing.T) {
	pantry := t.TempDir()
	writeRecipe(t, pantry, "acme.org", "package.yml", minimalYAML)
	set := setFrom(t, `
project = "other.org"
why     = "w"
edits   = [{ path = "a", set = 1 }]
`)
	r, err := LoadOverridden(set, pantry, "acme.org")
	if err != nil || r.Dependencies["openssl.org"] != "^1.1" {
		t.Fatalf("%v %v", r, err)
	}
	// And with no set at all.
	if _, err := LoadOverridden(nil, pantry, "acme.org"); err != nil {
		t.Fatal(err)
	}
}

// An HCL recipe goes through the same converter the client uses, so a recipe
// written either way is overridden identically.
func TestLoadOverriddenOnAnHCLRecipe(t *testing.T) {
	pantry := t.TempDir()
	writeRecipe(t, pantry, "acme.org", "package.hcl", `
distributable { url = "https://acme.org/{{version}}.tar.gz" }
dependencies = { "openssl.org" = "^1.1" }
build { script = "make install" }
provides = ["bin/acme"]
`)
	set := setFrom(t, `
project = "acme.org"
why     = "w"
edits   = [{ path = "dependencies[\"openssl.org\"]", set = "^3" }]
`)
	r, err := LoadOverridden(set, pantry, "acme.org")
	if err != nil {
		t.Fatal(err)
	}
	if r.Dependencies["openssl.org"] != "^3" {
		t.Errorf("got %v", r.Dependencies)
	}
}

func TestLoadOverriddenReportsFailures(t *testing.T) {
	pantry := t.TempDir()

	// No recipe at all.
	if _, err := LoadOverridden(nil, pantry, "absent.example"); err == nil {
		t.Error("a project with no recipe must be an error")
	}

	// An override whose premise is gone stops the load rather than quietly
	// building the unoverridden recipe — which is how a -Werror switch went
	// missing for weeks.
	writeRecipe(t, pantry, "acme.org", "package.yml", minimalYAML)
	gone := setFrom(t, `
project = "acme.org"
why     = "w"
edits   = [{ path = "build.script", from = "cmake", to = "cmake3" }]
`)
	if _, err := LoadOverridden(gone, pantry, "acme.org"); err == nil {
		t.Error("a premise that is gone must stop the load")
	}

	// An override that produces something a recipe may not say fails at the
	// schema, where every other recipe is checked.
	bad := setFrom(t, `
project = "acme.org"
why     = "w"
edits   = [{ path = "distributable", set = 7 }]
`)
	if _, err := LoadOverridden(bad, pantry, "acme.org"); err == nil {
		t.Error("an override that breaks the schema must fail")
	}

	// A recipe that is not the format its name claims.
	writeRecipe(t, pantry, "broken.example", "package.hcl", "distributable {")
	brokenSet := setFrom(t, `
project = "broken.example"
why     = "w"
edits   = [{ path = "a", set = 1 }]
`)
	if _, err := LoadOverridden(brokenSet, pantry, "broken.example"); err == nil {
		t.Error("a recipe that does not parse must fail")
	}

	// YAML that is not a mapping at all.
	writeRecipe(t, pantry, "list.example", "package.yml", "- one\n- two\n")
	listSet := setFrom(t, `
project = "list.example"
why     = "w"
edits   = [{ path = "a", set = 1 }]
`)
	if _, err := LoadOverridden(listSet, pantry, "list.example"); err == nil {
		t.Error("a recipe that is not a mapping must fail")
	}
}

// The seam: marshalling a document a parser produced cannot realistically
// fail, and swallowing the error would feed the schema an empty recipe.
func TestLoadOverriddenWhenTheDocumentCannotBeWrittenBack(t *testing.T) {
	pantry := t.TempDir()
	writeRecipe(t, pantry, "acme.org", "package.yml", minimalYAML)
	set := setFrom(t, `
project = "acme.org"
why     = "w"
edits   = [{ path = "dependencies[\"openssl.org\"]", set = "^3" }]
`)
	old := yamlMarshal
	t.Cleanup(func() { yamlMarshal = old })
	yamlMarshal = func(any) ([]byte, error) { return nil, os.ErrInvalid }
	if _, err := LoadOverridden(set, pantry, "acme.org"); err == nil {
		t.Error("want the marshal error, not an empty recipe")
	}
}
