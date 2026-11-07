package schema_test

import (
	"strings"
	"testing"

	"github.com/go-zpkg/zbk/pantry"
)

// ⛔⛔ A SCHEMA IS ONLY CLOSED IF SOMETHING FAILS WHEN IT IS REOPENED.
// `additionalProperties: false` is one word in a JSON file, and the corpus
// survey next door cannot see it: that test asks which keys a pantry uses,
// not whether the validator would refuse them. Without these cases, flipping
// a `false` back to `true` — or adding a sixth container and forgetting it —
// goes green.
//
// Each case is a recipe that differs from a valid one by exactly one key, so
// a failure names the container that stopped refusing.
func TestAnUnknownKeyIsRefused(t *testing.T) {
	const valid = `
distributable:
  url: https://example.org/x-{{version.raw}}.tar.gz
  strip-components: 1
versions:
  github: example/x
build:
  script:
    - make install
provides:
  - bin/x
`
	if err := pantry.Validate([]byte(valid)); err != nil {
		t.Fatalf("the control recipe does not validate, so every case below would "+
			"pass for the wrong reason: %v", err)
	}

	for _, c := range []struct{ what, yaml string }{
		{"root", valid + "\nnosuchkey: 1\n"},
		{"build", `
build:
  script: [make install]
  nosuchkey: 1
`},
		{"test", `
test:
  script: [./x --version]
  nosuchkey: 1
`},
		{"distributable", `
distributable:
  url: https://example.org/x.tar.gz
  nosuchkey: 1
`},
		{"versions", `
versions:
  github: example/x
  nosuchkey: 1
`},
	} {
		err := pantry.Validate([]byte(c.yaml))
		if err == nil {
			t.Errorf("%s: an unknown key validated — that container is open again, "+
				"and a recipe may declare something nothing implements", c.what)
			continue
		}
		if !strings.Contains(err.Error(), "schema validation failed") {
			t.Errorf("%s: refused for the wrong reason: %v", c.what, err)
		}
	}
}

// The two keys enumerated because the code has always honoured them. If
// either is dropped while the schema is closed, every recipe using it stops
// validating — 21 of the upstream corpus for `npm` — so they are pinned.
func TestTheKeysTheCodeHonoursAreAccepted(t *testing.T) {
	for _, c := range []struct{ what, yaml string }{
		{"distributable.sha", `
distributable:
  url: https://example.org/x.tar.gz
  sha: ${{url}}.sha256
`},
		{"versions.npm", `
versions:
  npm: left-pad
`},
	} {
		if err := pantry.Validate([]byte(c.yaml)); err != nil {
			t.Errorf("%s is honoured by the code and refused by the schema: %v", c.what, err)
		}
	}
}
