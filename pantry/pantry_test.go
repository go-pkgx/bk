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
