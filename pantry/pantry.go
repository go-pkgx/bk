// Package pantry parses and validates pkgx pantry package.yml recipes. The
// parsed Recipe is the shared representation that both the YAML front-end and
// the HCL2 front-end decode into, so a recipe written either way yields an
// identical build.
package pantry

import (
	"encoding/json"
	"fmt"

	"github.com/go-pkgx/bk/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// Recipe is a decoded package.yml. Fields use `any` where pkgx allows several
// shapes (a script may be a string or a list; env values a scalar or a list;
// deps a constraint or a platform-keyed map) — the build-script generator
// resolves those shapes against the build target. Unknown keys are retained in
// Extra so nothing is silently dropped.
type Recipe struct {
	Distributable any            `yaml:"distributable"`
	Versions      any            `yaml:"versions"`
	Dependencies  map[string]any `yaml:"dependencies"`
	Build         any            `yaml:"build"`
	Test          any            `yaml:"test"`
	Provides      any            `yaml:"provides"`
	Runtime       *Runtime       `yaml:"runtime"`
	Companions    map[string]any `yaml:"companions"`
	Platforms     any            `yaml:"platforms"`
	Warnings      any            `yaml:"warnings"`
	DisplayName   string         `yaml:"display-name"`
	Summary       string         `yaml:"summary"`
	Description   string         `yaml:"description"`
}

// Runtime is the environment a package exports to its consumers.
type Runtime struct {
	Env map[string]any `yaml:"env"`
}

// compiledSchema is the package.yml JSON Schema, compiled once at load.
var compiledSchema = must(compile(schema.PackageJSON))

// compile builds a jsonschema.Schema from raw JSON-Schema bytes.
func compile(data []byte) (*jsonschema.Schema, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	// AddResource does not validate an in-memory document (even a malformed
	// $id surfaces later, at Compile), so its error is unreachable here.
	_ = c.AddResource("package.schema.json", doc)
	return c.Compile("package.schema.json")
}

func must(s *jsonschema.Schema, err error) *jsonschema.Schema {
	if err != nil {
		panic("pantry: " + err.Error())
	}
	return s
}

// Validate checks raw package.yml bytes against the authoritative schema.
func Validate(data []byte) error {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("pantry: invalid YAML: %w", err)
	}
	doc = toJSONValue(doc)
	if err := compiledSchema.Validate(doc); err != nil {
		return fmt.Errorf("pantry: schema validation failed: %w", err)
	}
	return nil
}

// Parse decodes and validates package.yml bytes into a Recipe. It decodes into
// the typed struct first (so a type mismatch is reported precisely), then
// checks the whole document against the authoritative schema.
func Parse(data []byte) (*Recipe, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("pantry: decode: %w", err)
	}
	var r Recipe
	if err := root.Decode(&r); err != nil {
		return nil, fmt.Errorf("pantry: decode: %w", err)
	}
	// A list-form `versions:` enumerates candidate versions verbatim. yaml.v3
	// coerces unquoted numeric scalars when decoding into `any` (3.0 becomes
	// float64(3), losing the ".0"), which would corrupt the distributable URL
	// that interpolates {{version.raw}}. Re-read the sequence's raw scalar text
	// straight from the YAML node so each candidate keeps its exact source form.
	r.Versions = rawListVersions(&root, r.Versions)
	if err := Validate(data); err != nil {
		return nil, err
	}
	return &r, nil
}

// rawListVersions returns the verbatim scalar text of a list-form `versions:`
// sequence as a []any of strings, so a candidate like "3.0" is preserved
// exactly rather than coerced. For any other form (the github/url map, or an
// absent versions key) it returns decoded unchanged.
func rawListVersions(root *yaml.Node, decoded any) any {
	// A successful root.Decode into the Recipe struct guarantees a mapping
	// document; only the empty-document case has no content.
	if len(root.Content) == 0 {
		return decoded
	}
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "versions" {
			continue
		}
		node := m.Content[i+1]
		if node.Kind != yaml.SequenceNode {
			return decoded
		}
		// Only a SCALAR element carries verbatim text worth rescuing. A list can
		// also hold {url, match, strip} MAPPINGS -- kernel.org/linux-headers has
		// one per kernel series -- and a mapping node's .Value is the empty
		// string, so taking it turned every such source block into "" and the
		// recipe could no longer resolve a single version.
		dec, _ := decoded.([]any)
		out := make([]any, 0, len(node.Content))
		for i, c := range node.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
				continue
			}
			if i < len(dec) {
				out = append(out, dec[i])
			}
		}
		return out
	}
	return decoded
}

// toJSONValue normalises a yaml.v3-decoded value into the plain types the JSON
// Schema validator expects: map[string]any, []any, string, float64, bool, nil.
func toJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = toJSONValue(x)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = toJSONValue(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = toJSONValue(x)
		}
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	default:
		return v
	}
}
