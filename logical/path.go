package logical

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Path is a route to one place in a recipe document.
//
// Dots separate steps, and a step whose own name contains a dot or a slash —
// which every project name does — is written in brackets:
//
//	build.dependencies["crates.io/semverator"]
//	dependencies["openssl.org"]
//	build.env.ARGS
//
// Brackets take a quoted string, never a number. An index is a position, and a
// position is the thing this package refuses to depend on.
type Path []string

// ParsePath reads the form above. It is strict: an unterminated bracket, an
// empty step or a numeric index is an error at PARSE time, so a malformed
// override is refused before it can half-apply.
func ParsePath(s string) (Path, error) {
	if s == "" {
		return nil, fmt.Errorf("empty path")
	}
	var out Path
	for i := 0; i < len(s); {
		switch s[i] {
		case '[':
			end, step, err := bracketed(s, i)
			if err != nil {
				return nil, err
			}
			if err := representable(s, step); err != nil {
				return nil, err
			}
			out = append(out, step)
			i = end
		default:
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("empty step in %q at %d", s, i)
			}
			if err := representable(s, s[i:j]); err != nil {
				return nil, err
			}
			out = append(out, s[i:j])
			i = j
		}
		if i < len(s) && s[i] == '.' {
			i++
			if i == len(s) {
				return nil, fmt.Errorf("path %q ends in a dot", s)
			}
		}
	}
	return out, nil
}

// representable refuses a step that String could not render back.
//
// ⛔ THE WRITER AND THE READER HAVE TO AGREE, and they did not. String
// brackets a step that needs it with %q — Go's escaping — while bracketed
// finds the closing quote with strings.IndexByte and understands no escape at
// all. So a step holding a quote or a backslash printed in a spelling this
// parser cannot read:
//
//	ParsePath(`"`)   -> Path{`"`}, String() -> `["\""]` -> unterminated bracket
//	ParsePath(`\`)   -> Path{`\`}, String() -> `["\\"]` -> step `\\`, SILENTLY
//	                                                       two characters
//
// The second is the dangerous one: no error, a different path, and the
// diagnostic that names the place a person must fix names a different place.
//
// Found by FuzzParsePathRoundTrip in 0.03s. Teaching bracketed to decode
// escapes would be the other fix and the wrong one: a Path step is a recipe
// key, no recipe key contains a quote or a backslash, and that would be
// escape machinery written for input that cannot arrive. Refusing instead
// makes the unrepresentable step unparseable, so String ∘ ParsePath is a
// round trip by construction rather than by agreement.
// ⛔ AND THE RULE IS WIDER THAN QUOTES, which the fuzzer said next: a step
// holding a non-printable byte also round-trips badly, because %q spells it
// \x83 and so introduces the backslash this cannot read. The rule is
// therefore "what %q leaves alone" — printable, and valid UTF-8, which is
// every recipe key there has ever been.
func representable(s, step string) error {
	if !utf8.ValidString(step) {
		return fmt.Errorf("step %q in path %q is not valid UTF-8, and a path cannot spell those bytes", step, s)
	}
	for _, r := range step {
		switch {
		case r == '"' || r == '\\':
			return fmt.Errorf("step %q in path %q contains %q, which a path cannot spell", step, s, r)
		case !unicode.IsPrint(r):
			return fmt.Errorf("step %q in path %q contains %q, which a path cannot spell", step, s, r)
		}
	}
	return nil
}

// bracketed reads `["…"]` starting at s[i] == '[' and returns the index just
// past the closing bracket.
func bracketed(s string, i int) (int, string, error) {
	if i+1 >= len(s) || s[i+1] != '"' {
		return 0, "", fmt.Errorf("a bracket takes a quoted name, not a position: %q", s)
	}
	j := strings.IndexByte(s[i+2:], '"')
	if j < 0 {
		return 0, "", fmt.Errorf("unterminated quote in %q", s)
	}
	j += i + 2
	if j+1 >= len(s) || s[j+1] != ']' {
		return 0, "", fmt.Errorf("unterminated bracket in %q", s)
	}
	step := s[i+2 : j]
	if step == "" {
		return 0, "", fmt.Errorf("empty name in brackets in %q", s)
	}
	return j + 2, step, nil
}

// String renders a Path back, bracketing a step that needs it, so an error
// message names the place in the same spelling the override used.
func (p Path) String() string {
	var b strings.Builder
	for i, step := range p {
		if strings.ContainsAny(step, "./[]\"") {
			fmt.Fprintf(&b, "[%q]", step)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(step)
	}
	return b.String()
}
