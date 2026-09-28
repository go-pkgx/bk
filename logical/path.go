package logical

import (
	"fmt"
	"strings"
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
