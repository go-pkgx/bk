// Package hcl is an HCL2 front-end for pkgx pantry recipes. A package.hcl is
// decoded into the SAME pantry.Recipe as a package.yml — via the same schema
// validation — so a recipe written either way produces an identical build.
//
// Example (equivalent to the curl.se package.yml):
//
//	distributable {
//	  url              = "https://curl.se/download/curl-{{version}}.tar.bz2"
//	  strip-components = 1
//	}
//	dependencies = { "openssl.org" = "^1.1", "zlib.net" = "^1.2.11" }
//	build {
//	  script = ["./configure $ARGS", "make --jobs {{ hw.concurrency }} install"]
//	  env    = { ARGS = ["--prefix={{prefix}}", "--with-openssl"] }
//	}
//	provides = ["bin/curl", "bin/curl-config"]
//
// Blocks express nested maps (build, test, distributable); object attributes
// express maps whose keys aren't HCL identifiers (eg. "openssl.org").
package hcl

import (
	"fmt"

	"github.com/go-pkgx/bk/pantry"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
)

// Parse decodes a package.hcl into a validated pantry.Recipe.
func Parse(src []byte, filename string) (*pantry.Recipe, error) {
	doc, err := ToMap(src, filename)
	if err != nil {
		return nil, err
	}
	// round-trip through YAML so the exact same schema validation + struct
	// decode used for package.yml applies unchanged. ToMap only ever yields
	// JSON-safe values (string/float64/bool/nil/slice/map), which always
	// marshal, so the error is unreachable here.
	y, _ := yaml.Marshal(doc)
	return pantry.Parse(y)
}

// ToMap parses package.hcl into the generic map[string]any document shape that
// a package.yml decodes to.
func ToMap(src []byte, filename string) (map[string]any, error) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("hcl: parse: %s", diags.Error())
	}
	return bodyToMap(f.Body.(*hclsyntax.Body))
}

// bodyToMap converts an HCL body's attributes and nested blocks into a map.
func bodyToMap(body *hclsyntax.Body) (map[string]any, error) {
	out := make(map[string]any, len(body.Attributes)+len(body.Blocks))
	for name, attr := range body.Attributes {
		v, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("hcl: %s: %s", name, diags.Error())
		}
		g, err := ctyToGo(v)
		if err != nil {
			return nil, fmt.Errorf("hcl: %s: %w", name, err)
		}
		out[name] = g
	}
	for _, block := range body.Blocks {
		if _, exists := out[block.Type]; exists {
			return nil, fmt.Errorf("hcl: duplicate key %q (block and/or attribute)", block.Type)
		}
		m, err := bodyToMap(block.Body)
		if err != nil {
			return nil, err
		}
		out[block.Type] = m
	}
	return out, nil
}

// ctyToGo converts a cty.Value into the plain Go value a YAML decode would
// yield: string, float64, bool, nil, []any, map[string]any.
func ctyToGo(v cty.Value) (any, error) {
	if v.IsNull() {
		return nil, nil
	}
	t := v.Type()
	switch {
	case t == cty.String:
		return v.AsString(), nil
	case t == cty.Bool:
		return v.True(), nil
	case t == cty.Number:
		f, _ := v.AsBigFloat().Float64()
		return f, nil
	case t.IsTupleType(), t.IsListType(), t.IsSetType():
		var out []any
		for _, e := range v.AsValueSlice() {
			g, err := ctyToGo(e)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case t.IsObjectType(), t.IsMapType():
		out := make(map[string]any)
		for k, e := range v.AsValueMap() {
			g, err := ctyToGo(e)
			if err != nil {
				return nil, err
			}
			out[k] = g
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported HCL value type %s", t.FriendlyName())
	}
}
