package hcl

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-pkgx/bk/pantry"
	"gopkg.in/yaml.v3"
)

// Emit renders a recipe document as package.hcl text.
//
// It is the inverse of ToMap, and the only thing that makes it trustworthy is
// that the two are checked against each other: Convert parses what this
// produces and refuses to hand it back unless the result is the document it
// started from. A converter nobody checks is a rewrite.
//
// Shape follows this package's own convention. A map whose keys are all HCL
// identifiers becomes a BLOCK, because `build { script = … }` is what a reader
// expects; a map with a key like "openssl.org" cannot — an attribute name must
// be an identifier — so it becomes an object attribute with quoted keys.
func Emit(doc map[string]any) string {
	var b strings.Builder
	emitBody(&b, doc, "")
	return b.String()
}

func emitBody(b *strings.Builder, m map[string]any, indent string) {
	// Blocks last, so the short attributes a reader scans first are not buried
	// under a build script.
	var attrs, blocks []string
	for k, v := range m {
		if asBlock(k, v) {
			blocks = append(blocks, k)
		} else {
			attrs = append(attrs, k)
		}
	}
	sort.Strings(attrs)
	sort.Strings(blocks)
	for _, k := range attrs {
		fmt.Fprintf(b, "%s%s = %s\n", indent, k, expr(m[k], indent))
	}
	for _, k := range blocks {
		fmt.Fprintf(b, "\n%s%s {\n", indent, k)
		emitBody(b, m[k].(map[string]any), indent+"  ")
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

// asBlock decides between `k { … }` and `k = { … }`.
//
// Only a map can be a block, only under an identifier name, and only when
// every key inside is an identifier too — otherwise the body would need an
// attribute called "openssl.org", which HCL cannot express.
func asBlock(k string, v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 || !ident(k) {
		return false
	}
	for kk := range m {
		if !ident(kk) {
			return false
		}
	}
	return true
}

// ident reports whether a name can be written bare in HCL.
func ident(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case (r >= '0' && r <= '9' || r == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

func expr(v any, indent string) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("%t", x)
	case int:
		return fmt.Sprintf("%d", x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case string:
		return str(x, indent)
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		var b strings.Builder
		b.WriteString("[\n")
		for _, e := range x {
			rendered := expr(e, indent+"  ")
			b.WriteString(indent + "  " + rendered)
			// A heredoc's terminator must be alone on its line, so the comma
			// that separates list items cannot follow it. `EOT,` is where the
			// first run of this refused 15 recipes with "Unterminated template
			// string" — the parser reads EOT, as a different word and never
			// finds the end.
			if isHeredoc(rendered) {
				b.WriteString("\n,\n")
				continue
			}
			b.WriteString(",\n")
		}
		b.WriteString(indent + "]")
		return b.String()
	case map[string]any:
		if len(x) == 0 {
			return "{}"
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			name := k
			if !ident(k) {
				name = quote(k)
			}
			parts = append(parts, fmt.Sprintf("%s  %s = %s", indent, name, expr(x[k], indent+"  ")))
		}
		return "{\n" + strings.Join(parts, "\n") + "\n" + indent + "}"
	default:
		// A YAML decode yields only the shapes above. Anything else means the
		// document holds something this converter has never seen, and guessing
		// at it would produce HCL that parses into something different.
		return quote(fmt.Sprint(v))
	}
}

// str renders a string, as a heredoc when it has newlines.
//
// A recipe's script is the reason: rendered as one escaped line it is
// unreadable, and the point of the conversion is a file people edit.
func str(s, indent string) string {
	// A heredoc can carry exactly one shape of string: one that ends with a
	// newline, written verbatim. Anything else has to be quoted.
	//
	// The first version used <<- and indented the body to match its
	// surroundings, which reads far better and is wrong: <<- strips the
	// COMMON leading whitespace, so a fixture whose own C code is indented
	// comes back with that indentation removed. Twenty-eight recipes were
	// refused for it, every one a script or a fixture, and the refusal is the
	// only reason none of them shipped altered.
	//
	// So the body sits at column 0 and the terminator with it. It is uglier
	// than an indented heredoc and it is the same bytes.
	if !strings.HasSuffix(s, "\n") || !strings.Contains(s, "\n") {
		return quote(s)
	}
	tag := heredocTag(s)
	var b strings.Builder
	b.WriteString("<<" + tag + "\n")
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		b.WriteString(escapeTemplate(line) + "\n")
	}
	b.WriteString(tag)
	return b.String()
}

// heredocTag picks a terminator the body does not contain.
//
// A heredoc ends at the first line equal to its tag, so a script with a line
// reading exactly EOT would end it early and the rest of the script would be
// read as HCL. Found by covering Convert's "does not parse" guard, which
// needed an input that breaks the emitter — and this was one.
func heredocTag(body string) string {
	tag := "EOT"
	for lineIs(body, tag) {
		tag += "_"
	}
	return tag
}

// lineIs reports whether any line of s is exactly tag, ignoring the trailing
// whitespace HCL also ignores when matching a terminator.
func lineIs(s, tag string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimRight(line, " \t") == tag {
			return true
		}
	}
	return false
}

// heredocEnd is what a rendered heredoc ends with, so a list can tell one
// apart from an ordinary expression and put its comma on the next line.
const heredocEnd = "EOT"

// isHeredoc reports whether a rendered expression is a heredoc, whatever
// terminator it had to choose.
func isHeredoc(rendered string) bool {
	return strings.HasPrefix(rendered, "<<"+heredocEnd)
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return escapeTemplate(b.String())
}

// escapeTemplate defuses HCL's template syntax.
//
// This is the trap the whole conversion turns on. HCL reads `${…}` as an
// interpolation and `%{…}` as a directive, and a recipe's script is FULL of
// shell expansions — `${prefix}`, `${LDFLAGS}`, `$(…)` is safe but `${…}` is
// not. Left alone, HCL either fails to parse or, worse, evaluates something
// and silently changes the script. Doubling the sigil is how HCL spells a
// literal one.
func escapeTemplate(s string) string {
	s = strings.ReplaceAll(s, "${", "$${")
	return strings.ReplaceAll(s, "%{", "%%{")
}

// emitFn is a seam. The guard below — "the hcl this produced does not parse"
// — protects against a defect in Emit, so nothing reachable through the public
// API can trigger it once Emit is correct. It is the check that caught the
// heredoc terminator colliding with a script's own EOT line, so it stays, and
// a test reaches it the way this codebase reaches its other unreachable
// branches.
var emitFn = Emit

// Convert turns a package.yml into package.hcl text, and refuses to return
// text that does not parse back into the same recipe.
//
// The check is the feature. Emit is a hand-written renderer of a document
// shape with heredocs, quoted keys and a template syntax that has to be
// defused; the chance of it being right by inspection is poor, and the cost of
// a silent mistake is a recipe that builds something else. So the caller never
// sees output that has not been read back.
//
// Compared as RECIPES rather than as documents: that is what a build consumes,
// and it is the same comparison pantry.Parse's own schema validation ends at.
func Convert(yamlSrc []byte) ([]byte, error) {
	// Decode first, validate second: a file that is not YAML at all fails
	// here with something a reader can act on, rather than as a schema
	// complaint about a document that was never parsed.
	var doc map[string]any
	if err := yaml.Unmarshal(yamlSrc, &doc); err != nil {
		return nil, fmt.Errorf("the yaml does not decode: %w", err)
	}
	want, err := pantry.Parse(yamlSrc)
	if err != nil {
		return nil, fmt.Errorf("the yaml does not parse: %w", err)
	}
	out := []byte(emitFn(doc))
	got, err := Parse(out, "package.hcl")
	if err != nil {
		return nil, fmt.Errorf("the hcl this produced does not parse: %w", err)
	}
	if !reflect.DeepEqual(want, got) {
		// Naming the PATH, not printing both recipes. The first version dumped
		// them with %#v and they came out character-identical, because %#v
		// renders int(1) and float64(1) the same way — the difference was a
		// type, and the message could not show it.
		return nil, fmt.Errorf("the hcl this produced is a DIFFERENT recipe: %s", firstDiff(want, got, ""))
	}
	return out, nil
}

// firstDiff walks two decoded recipes and describes the first place they part,
// with the TYPES — which is where the differences live. A YAML decode gives
// int for a whole number; a cty value gives float64, and %#v prints both as
// "1".
func firstDiff(a, b any, path string) string {
	if path == "" {
		path = "."
	}
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.IsValid() != rb.IsValid() {
		return fmt.Sprintf("%s: one side is absent", path)
	}
	if !ra.IsValid() {
		return ""
	}
	if ra.Kind() == reflect.Ptr && rb.Kind() == reflect.Ptr {
		// Both nil is EQUAL. The first version reported "<nil> vs <nil>" as a
		// difference, so every recipe with an absent `runtime:` — which is
		// most of them — would have been refused for differing from itself.
		// A test comparing a recipe with itself is what found it.
		if ra.IsNil() && rb.IsNil() {
			return ""
		}
		if ra.IsNil() || rb.IsNil() {
			return fmt.Sprintf("%s: %v vs %v", path, a, b)
		}
		return firstDiff(ra.Elem().Interface(), rb.Elem().Interface(), path)
	}
	if ra.Kind() == reflect.Struct && rb.Kind() == reflect.Struct && ra.Type() == rb.Type() {
		for i := 0; i < ra.NumField(); i++ {
			if !ra.Type().Field(i).IsExported() {
				continue
			}
			if d := firstDiff(ra.Field(i).Interface(), rb.Field(i).Interface(), path+"."+ra.Type().Field(i).Name); d != "" {
				return d
			}
		}
		return ""
	}
	if ra.Type() != rb.Type() {
		return fmt.Sprintf("%s: %T(%v) from yaml, %T(%v) from hcl", path, a, a, b, b)
	}
	switch ra.Kind() {
	case reflect.Map:
		for _, k := range ra.MapKeys() {
			vb := rb.MapIndex(k)
			if !vb.IsValid() {
				return fmt.Sprintf("%s[%v]: present in the yaml, absent from the hcl", path, k)
			}
			if d := firstDiff(ra.MapIndex(k).Interface(), vb.Interface(), fmt.Sprintf("%s[%v]", path, k)); d != "" {
				return d
			}
		}
		for _, k := range rb.MapKeys() {
			if !ra.MapIndex(k).IsValid() {
				return fmt.Sprintf("%s[%v]: the hcl invented it", path, k)
			}
		}
		return ""
	case reflect.Slice:
		if ra.Len() != rb.Len() {
			return fmt.Sprintf("%s: %d items from yaml, %d from hcl", path, ra.Len(), rb.Len())
		}
		for i := 0; i < ra.Len(); i++ {
			if d := firstDiff(ra.Index(i).Interface(), rb.Index(i).Interface(), fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Sprintf("%s: %T(%v) from yaml, %T(%v) from hcl", path, a, a, b, b)
	}
	return ""
}
