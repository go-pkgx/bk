package logical

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Emit writes an override back as HCL.
//
// It exists for the conversion: 242 unified diffs become files nobody typed,
// and the only way that is a migration rather than a rewrite is if the output
// can be read back and checked. So the property this owes is a ROUND TRIP —
// Emit then Parse gives the same operations — and above it the real one, that
// applying them reproduces what the diff produced.
//
// Derived operations come out as `edits`, one verb per entry, because that is
// faithfully what was derived and the `path` leaves no doubt what is touched.
// The `merge` block stays for files a person writes, where a recipe fragment
// reads better than a list of paths.
func Emit(o *Override) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "project = %s\n", hclString(o.Project))
	fmt.Fprintf(&b, "why     = %s\n", hclString(o.Why))
	if len(o.Ops) == 0 {
		return []byte(b.String())
	}
	b.WriteString("\nedits = [\n")
	for _, op := range o.Ops {
		b.WriteString("  {\n")
		if op.Why != "" && op.Why != o.Why {
			fmt.Fprintf(&b, "    why  = %s\n", hclString(op.Why))
		}
		fmt.Fprintf(&b, "    path = %s\n", hclString(op.Path.String()))
		switch {
		case op.Remove:
			b.WriteString("    remove = true\n")
		case op.Substitute:
			fmt.Fprintf(&b, "    from = %s\n", hclString(op.From))
			fmt.Fprintf(&b, "    to   = %s\n", hclString(op.To))
		case op.Append != nil:
			fmt.Fprintf(&b, "    append = %s\n", hclValue(op.Append, 4))
		case op.Prepend != nil:
			fmt.Fprintf(&b, "    prepend = %s\n", hclValue(op.Prepend, 4))
		default:
			fmt.Fprintf(&b, "    set = %s\n", hclValue(op.Set, 4))
			if op.HasExpect {
				fmt.Fprintf(&b, "    expect = %s\n", hclValue(op.Expect, 4))
			}
		}
		b.WriteString("  },\n")
	}
	b.WriteString("]\n")
	return []byte(b.String())
}

// hclString quotes a string, or writes a heredoc when it holds newlines —
// which a build script always does, and which a quoted string would render as
// one unreadable line of \n.
func hclString(s string) string {
	if !strings.Contains(s, "\n") {
		return quote(s)
	}
	// A heredoc ends at a line that is exactly the marker, so a marker the
	// text itself contains would close it early. Lengthen until it does not.
	marker := "EOT"
	for strings.Contains(s, "\n"+marker) || strings.HasPrefix(s, marker) {
		marker += "T"
	}
	body := s
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return "<<" + marker + "\n" + escapeTemplates(body) + marker
}

// quote renders a one-line string. Go's %q escapes what HCL needs escaped in a
// quoted string, and then the template introducers have to be doubled on top:
// HCL reads ${…} and %{…} inside quotes, and a recipe is full of $ARGS and
// ${PKGX_DIR:-$HOME/.pkgx}.
func quote(s string) string {
	return escapeTemplates(fmt.Sprintf("%q", s))
}

// escapeTemplates doubles HCL's two template introducers so they come back as
// themselves. It is deliberately NOT applied to {{version}} — HCL does not read
// double braces, the recipe does, and escaping them here would change what the
// builder substitutes.
func escapeTemplates(s string) string {
	s = strings.ReplaceAll(s, "${", "$${")
	return strings.ReplaceAll(s, "%{", "%%{")
}

// hclValue renders any decoded value. Maps are written with quoted keys, which
// is the only form that takes a project name.
func hclValue(v any, indent int) string {
	pad := strings.Repeat(" ", indent)
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return hclString(t)
	case bool:
		return fmt.Sprintf("%t", t)
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		var b strings.Builder
		b.WriteString("[\n")
		for _, e := range t {
			v := hclValue(e, indent+2)
			// A heredoc closes only on a line that is EXACTLY its marker, so
			// the separating comma cannot share that line. Written the obvious
			// way, facebook.com/fbthrift emitted a file whose last heredoc
			// never closed and HCL reported an unterminated string four lines
			// past the end of the file.
			if strings.HasPrefix(v, "<<") {
				fmt.Fprintf(&b, "%s  %s\n%s  ,\n", pad, v, pad)
				continue
			}
			fmt.Fprintf(&b, "%s  %s,\n", pad, v)
		}
		fmt.Fprintf(&b, "%s]", pad)
		return b.String()
	case map[string]any:
		if len(t) == 0 {
			return "{}"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "%s  %s = %s\n", pad, quote(k), hclValue(t[k], indent+2))
		}
		fmt.Fprintf(&b, "%s}", pad)
		return b.String()
	default:
		return quote(fmt.Sprint(v))
	}
}
