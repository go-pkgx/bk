package pantry

import (
	"errors"
	"strings"
	"testing"
)

const curlRecipe = `
distributable:
  url: https://curl.se/download/curl-{{version}}.tar.bz2
  strip-components: 1
display-name: cURL
versions:
  github: curl/curl/releases
  strip: /^curl /
dependencies:
  openssl.org: ^1.1
  zlib.net: ^1.2.11
build:
  script:
    - ./configure $ARGS
    - make --jobs {{ hw.concurrency }} install
  env:
    ARGS:
      - --prefix={{prefix}}
      - --with-openssl
test:
  - curl -i pkgx.sh
provides:
  - bin/curl
  - bin/curl-config
`

func TestValidateAndParse(t *testing.T) {
	if err := Validate([]byte(curlRecipe)); err != nil {
		t.Fatalf("valid recipe rejected: %v", err)
	}
	r, err := Parse([]byte(curlRecipe))
	if err != nil {
		t.Fatal(err)
	}
	if r.DisplayName != "cURL" {
		t.Errorf("DisplayName = %q", r.DisplayName)
	}
	if _, ok := r.Dependencies["openssl.org"]; !ok {
		t.Errorf("deps = %v", r.Dependencies)
	}
	if r.Distributable == nil || r.Build == nil || r.Provides == nil {
		t.Error("expected distributable/build/provides populated")
	}
}

func TestValidateRejectsBadSchema(t *testing.T) {
	// dependencies must be a map/null; a string violates the schema
	bad := "dependencies: not-a-map\nbuild: echo hi\ntest: true\n"
	err := Validate([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "schema validation") {
		t.Fatalf("expected schema error, got %v", err)
	}
}

func TestValidateRejectsBadYAML(t *testing.T) {
	bad := "build: [unterminated\n"
	if err := Validate([]byte(bad)); err == nil || !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("expected YAML error, got %v", err)
	}
}

func TestParseDecodeError(t *testing.T) {
	// valid YAML that fails the typed decode: display-name is a string field
	// but given a list. Parse decodes first, so this surfaces a decode error.
	if _, err := Parse([]byte("display-name: [a, b]\n")); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestParseSchemaError(t *testing.T) {
	// decodes cleanly (provides is `any`) but fails the schema (a number is not
	// a valid provides) — exercises Parse's validation branch after decode.
	if _, err := Parse([]byte("provides: 123\n")); err == nil || !strings.Contains(err.Error(), "schema validation") {
		t.Errorf("expected schema error, got %v", err)
	}
}

func TestParseListVersionsPreservesRawScalar(t *testing.T) {
	// The crux: yaml.v3 coerces the unquoted 3.0 to float64(3) when decoding
	// into `any`, which would render as "3". Parse must re-read the sequence's
	// verbatim scalar text so the candidate stays exactly "3.0", and likewise
	// keep an int "7" and a dotted "7.0.6" intact.
	src := "versions:\n  - 3.0\n  - 7\n  - 7.0.6\nbuild: echo hi\ntest: true\n"
	r, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := r.Versions.([]any)
	if !ok {
		t.Fatalf("Versions = %T; want []any", r.Versions)
	}
	want := []any{"3.0", "7", "7.0.6"}
	if len(list) != len(want) {
		t.Fatalf("Versions = %v; want %v", list, want)
	}
	for i, w := range want {
		if list[i] != w {
			t.Errorf("Versions[%d] = %#v; want %#v (raw scalar text)", i, list[i], w)
		}
	}
}

func TestParseMapVersionsUnchanged(t *testing.T) {
	// The github/url map form must pass through as a map, untouched.
	r, err := Parse([]byte(curlRecipe))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r.Versions.(map[string]any)
	if !ok || m["github"] != "curl/curl/releases" {
		t.Errorf("Versions = %#v; want github map", r.Versions)
	}
}

func TestParseNoVersionsKey(t *testing.T) {
	// A recipe with no versions key leaves Versions nil (loop finds nothing).
	r, err := Parse([]byte("build: echo hi\ntest: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Versions != nil {
		t.Errorf("Versions = %#v; want nil", r.Versions)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	// An empty document decodes without error and has no content node, so the
	// raw-scalar extraction must short-circuit (not index a nil node) and let
	// the schema step reject the null document normally — no panic.
	if _, err := Parse([]byte("")); err == nil || !strings.Contains(err.Error(), "schema validation") {
		t.Errorf("empty document: expected schema error, got %v", err)
	}
}

func TestParseMalformedYAML(t *testing.T) {
	// Malformed YAML fails the first (node) unmarshal, not the struct decode.
	if _, err := Parse([]byte("build: [unterminated\n")); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestToJSONValue(t *testing.T) {
	in := map[string]any{
		"a": map[any]any{1: "x", "b": []any{int(2), int64(3), "s", true, nil}},
	}
	out := toJSONValue(in).(map[string]any)
	sub := out["a"].(map[string]any)
	if sub["1"] != "x" {
		t.Errorf("map[any]any key not stringified: %v", sub)
	}
	arr := sub["b"].([]any)
	if arr[0] != float64(2) || arr[1] != float64(3) {
		t.Errorf("ints not converted to float64: %v", arr)
	}
	if arr[2] != "s" || arr[3] != true || arr[4] != nil {
		t.Errorf("scalars mangled: %v", arr)
	}
}

func TestCompileErrors(t *testing.T) {
	// invalid JSON
	if _, err := compile([]byte("{not json")); err == nil {
		t.Error("expected JSON error")
	}
	// valid JSON but a schema the compiler rejects (dangling $ref)
	if _, err := compile([]byte(`{"$ref":"#/$defs/missing"}`)); err == nil {
		t.Error("expected compile error on dangling $ref")
	}
	// valid JSON but not a schema document (a bare number) → AddResource rejects
	if _, err := compile([]byte(`42`)); err == nil {
		t.Error("expected AddResource error on a non-object schema doc")
	}
	// the embedded schema compiles cleanly
	if _, err := compile([]byte(`{"type":"object"}`)); err != nil {
		t.Errorf("valid schema failed: %v", err)
	}
}

func TestMustPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("must did not panic on error")
		}
	}()
	must(nil, errors.New("boom"))
}

// TestParseVersionsListOfSourceBlocks: a list-form `versions:` may hold source
// BLOCKS, not just literal versions — kernel.org/linux-headers lists one
// {url, match, strip} per kernel series. Rescuing the verbatim scalar text of
// every element (to keep "3.0" from becoming 3) turned each mapping into the
// empty string, and the recipe could no longer resolve a single version.
func TestParseVersionsListOfSourceBlocks(t *testing.T) {
	rec, err := Parse([]byte("distributable:\n  url: https://x/y-{{version}}.tar.xz\nversions:\n" +
		"  - url: \"https://cdn.example/v6.x/\"\n    match: /linux-\\d+\\.tar\\.xz/\n    strip:\n      - /linux-/\n" +
		"  - url: \"https://cdn.example/v5.x/\"\n    match: /linux-\\d+\\.tar\\.xz/\nbuild: make\n"))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := rec.Versions.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("versions = %#v", rec.Versions)
	}
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("element %d is %T, want the source block intact", i, e)
		}
		if m["url"] == nil || m["match"] == nil {
			t.Errorf("element %d lost its keys: %#v", i, m)
		}
	}
	// …and a list of literal versions still keeps its verbatim text, which is
	// what the rescue existed for: "3.0" must not become 3.
	rec, err = Parse([]byte("distributable:\n  url: https://x\nversions:\n  - 3.0\n  - 3.1.4\nbuild: make\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Versions.([]any); got[0] != "3.0" || got[1] != "3.1.4" {
		t.Errorf("literal versions = %#v, want the exact source text", got)
	}
}
