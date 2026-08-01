package hcl

import (
	"reflect"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/pantry"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const curlHCL = `
distributable {
  url              = "https://curl.se/download/curl-{{version}}.tar.bz2"
  strip-components = 1
}
display-name = "cURL"
versions {
  github = "curl/curl/releases"
  strip  = "/^curl /"
}
dependencies = { "openssl.org" = "^1.1", "zlib.net" = "^1.2.11" }
build {
  script = ["./configure $ARGS", "make --jobs {{ hw.concurrency }} install"]
  env    = { ARGS = ["--prefix={{prefix}}", "--with-openssl"] }
}
test     = ["curl -i pkgx.sh"]
provides = ["bin/curl", "bin/curl-config"]
`

const curlYAML = `
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

func TestHCLEquivalentToYAML(t *testing.T) {
	fromHCL, err := Parse([]byte(curlHCL), "package.hcl")
	if err != nil {
		t.Fatalf("HCL parse: %v", err)
	}
	fromYAML, err := pantry.Parse([]byte(curlYAML))
	if err != nil {
		t.Fatalf("YAML parse: %v", err)
	}
	if !reflect.DeepEqual(fromHCL, fromYAML) {
		t.Errorf("HCL and YAML recipes differ:\n hcl=%#v\nyaml=%#v", fromHCL, fromYAML)
	}
	if fromHCL.DisplayName != "cURL" {
		t.Errorf("DisplayName=%q", fromHCL.DisplayName)
	}
}

func TestParseSyntaxError(t *testing.T) {
	if _, err := Parse([]byte("build { script = "), "x.hcl"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestParseEvalError(t *testing.T) {
	// a bare reference has no value in a nil eval context
	if _, err := ToMap([]byte("x = undefined_ref\n"), "x.hcl"); err == nil {
		t.Error("expected eval error on undefined reference")
	}
}

func TestParseDuplicateKey(t *testing.T) {
	src := "build { script = [\"a\"] }\nbuild { script = [\"b\"] }\n"
	if _, err := ToMap([]byte(src), "x.hcl"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-key error, got %v", err)
	}
}

func TestParseSchemaInvalid(t *testing.T) {
	// decodes fine but violates the schema (provides must not be a number)
	if _, err := Parse([]byte("provides = 123\n"), "x.hcl"); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("expected schema error, got %v", err)
	}
}

func TestBlockRecursionError(t *testing.T) {
	// an eval error inside a nested block propagates out of bodyToMap
	if _, err := ToMap([]byte("build {\n  x = undef_ref\n}\n"), "x.hcl"); err == nil {
		t.Error("expected error from block body")
	}
}

func TestBodyToMapAttributeConvertError(t *testing.T) {
	// craft a body whose attribute evaluates to an unsupported (capsule) value,
	// exercising bodyToMap's ctyToGo error branch (unreachable via HCL syntax).
	body := &hclsyntax.Body{
		Attributes: hclsyntax.Attributes{
			"x": &hclsyntax.Attribute{Name: "x", Expr: &hclsyntax.LiteralValueExpr{Val: capsule()}},
		},
	}
	if _, err := bodyToMap(body); err == nil {
		t.Error("expected ctyToGo error on a capsule-valued attribute")
	}
}

func TestParseNestedBlock(t *testing.T) {
	// a block nested in a block round-trips as a nested map
	m, err := ToMap([]byte("build {\n  env { A = \"1\" }\n}\n"), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	build := m["build"].(map[string]any)
	if env, ok := build["env"].(map[string]any); !ok || env["A"] != "1" {
		t.Errorf("nested block = %#v", build)
	}
}

func TestCtyToGoScalars(t *testing.T) {
	cases := []struct {
		v    cty.Value
		want any
	}{
		{cty.NullVal(cty.String), nil},
		{cty.StringVal("s"), "s"},
		{cty.True, true},
		{cty.NumberIntVal(7), float64(7)},
	}
	for _, c := range cases {
		got, err := ctyToGo(c.v)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ctyToGo(%#v)=%v,%v want %v", c.v, got, err, c.want)
		}
	}
}

func TestCtyToGoCollections(t *testing.T) {
	tup := cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.NumberIntVal(2)})
	got, err := ctyToGo(tup)
	if err != nil || !reflect.DeepEqual(got, []any{"a", float64(2)}) {
		t.Errorf("tuple = %v %v", got, err)
	}
	set := cty.SetVal([]cty.Value{cty.StringVal("x")})
	if g, err := ctyToGo(set); err != nil || !reflect.DeepEqual(g, []any{"x"}) {
		t.Errorf("set = %v %v", g, err)
	}
	obj := cty.ObjectVal(map[string]cty.Value{"k": cty.StringVal("v")})
	if g, err := ctyToGo(obj); err != nil || !reflect.DeepEqual(g, map[string]any{"k": "v"}) {
		t.Errorf("object = %v %v", g, err)
	}
	m := cty.MapVal(map[string]cty.Value{"k": cty.StringVal("v")})
	if g, err := ctyToGo(m); err != nil || !reflect.DeepEqual(g, map[string]any{"k": "v"}) {
		t.Errorf("map = %v %v", g, err)
	}
}

func TestCtyToGoUnsupported(t *testing.T) {
	cap := capsule()
	// standalone unsupported type
	if _, err := ctyToGo(cap); err == nil {
		t.Error("expected unsupported-type error")
	}
	// inside a tuple → recursion error path
	if _, err := ctyToGo(cty.TupleVal([]cty.Value{cap})); err == nil {
		t.Error("expected tuple recursion error")
	}
	// inside an object → recursion error path
	if _, err := ctyToGo(cty.ObjectVal(map[string]cty.Value{"k": cap})); err == nil {
		t.Error("expected object recursion error")
	}
}

// capsule returns a cty value of a capsule type, which ctyToGo does not support.
func capsule() cty.Value {
	ty := cty.Capsule("weird", reflect.TypeOf(struct{ X int }{}))
	return cty.CapsuleVal(ty, &struct{ X int }{})
}
