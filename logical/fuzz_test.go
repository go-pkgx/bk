package logical

import (
	"slices"
	"strings"
	"testing"
)

// AN OVERRIDE IS A FILE THAT REWRITES A RECIPE, AND NOTHING ELSE CHECKS IT.
//
// bk had no Fuzz targets anywhere before this file, while this package parses
// the format that decides what the factory actually builds. The failure mode
// is not a crash: it is an override that half-applies, or one whose Path
// prints in a spelling the parser will not read back — because the printed
// spelling is what every diagnostic shows a person who has to fix it.
//
// The property is the ROUND TRIP. ParsePath reads a path, String renders one,
// and the two have to agree: an encoder must encode what its reader decodes.
// A target that only checked "does not panic" would pass today and tell
// nobody that the two halves disagree.
func FuzzParsePathRoundTrip(f *testing.F) {
	f.Add(`dependencies["openssl.org"]`)
	f.Add(`build.dependencies["crates.io/semverator"]`)
	f.Add("build.env.ARGS")
	f.Add("versions")
	f.Add(`["openssl.org"]`)
	f.Add(`a["b.c"].d`)
	f.Add("")
	f.Add("[")
	f.Add(`["unterminated`)
	f.Add("a..b")
	f.Add("a[0]")
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParsePath(s)
		if err != nil {
			return // refused at parse time, which is this parser's contract
		}
		if len(p) == 0 {
			t.Fatalf("ParsePath(%q) returned an empty path and no error", s)
		}
		for i, step := range p {
			if step == "" {
				t.Fatalf("ParsePath(%q) step %d is empty", s, i)
			}
		}
		// ⛔ THE RENDERED FORM MUST PARSE BACK TO THE SAME PATH. It is what
		// error messages print, and what a person copies back into an
		// override.
		again, err := ParsePath(p.String())
		if err != nil {
			t.Fatalf("ParsePath(%q) = %v, whose String() %q does not parse: %v", s, p, p.String(), err)
		}
		if !slices.Equal(p, again) {
			t.Fatalf("round trip changed the path: %q -> %v -> %q -> %v", s, p, p.String(), again)
		}
	})
}

// The three reasons a step cannot be spelled, named rather than left to the
// fuzzer's corpus — a rule with a stated reason deserves a test that says the
// reason, and the corpus happens to cover two of these through one entry.
func TestAStepThatCannotBeSpelledIsRefused(t *testing.T) {
	for _, c := range []struct{ what, path string }{
		{"a quote, which String would escape as \\\"", `"`},
		{"a backslash, which round-trips to two characters", `\`},
		{"a lone continuation byte, which is not UTF-8", "[\".\x83\"]"},
		{"a control character, which String would escape as \\x01", "\x01"},
		{"a control character inside brackets", "[\"a\x01b\"]"},
	} {
		if _, err := ParsePath(c.path); err == nil {
			t.Errorf("ParsePath(%q) was accepted; it holds %s", c.path, c.what)
		}
	}
	// The control: every shape a real override uses must still parse, or a
	// rule that refused everything would pass the cases above too.
	for _, ok := range []string{
		"versions",
		"build.env.ARGS",
		`dependencies["openssl.org"]`,
		`build.dependencies["crates.io/semverator"]`,
		`dependencies.linux["gnu.org/gcc/libstdcxx"]`,
	} {
		p, err := ParsePath(ok)
		if err != nil {
			t.Errorf("ParsePath(%q) was refused: %v", ok, err)
			continue
		}
		if again, err := ParsePath(p.String()); err != nil {
			t.Errorf("ParsePath(%q).String() = %q does not parse: %v", ok, p.String(), err)
		} else if !slices.Equal(p, again) {
			t.Errorf("%q does not round trip: %v -> %q -> %v", ok, p, p.String(), again)
		}
	}
}

// The override document itself. If it parses, every operation must name a
// place that can be read back — an Op with an unreadable Path is one that
// cannot be reported when it fails to apply, which is the silent
// half-application this package exists to prevent.
func FuzzParseOverride(f *testing.F) {
	f.Add(`project = "a.org"
why     = "x"
edits = [
  {
    path = "dependencies[\"openssl.org\"]"
    set  = "^3"
  },
]
`)
	f.Add(`project = "a.org"
why = "x"
edits = []
`)
	f.Add(`project = "a.org"
why = "x"
edits = [
  {
    path = "build.env.CFLAGS"
    set  = "$CFLAGS -w"
  },
]
`)
	f.Add("project = \n")
	f.Add("")
	f.Fuzz(func(t *testing.T, src string) {
		o, err := Parse([]byte(src), "fuzz.hcl")
		if err != nil {
			return
		}
		if o == nil {
			t.Fatal("Parse returned no override and no error")
		}
		for i, op := range o.Ops {
			if len(op.Path) == 0 {
				t.Fatalf("op %d has an empty path", i)
			}
			s := op.Path.String()
			back, perr := ParsePath(s)
			if perr != nil {
				t.Fatalf("op %d path %v prints as %q, which does not parse: %v", i, op.Path, s, perr)
			}
			if !slices.Equal(op.Path, back) {
				t.Fatalf("op %d path does not round trip: %v -> %q -> %v", i, op.Path, s, back)
			}
			// The reason is shown to whoever has to act on a skip, so it has
			// to be printable.
			if strings.ContainsAny(op.Why, "\x00\x1b\a") {
				t.Fatalf("op %d why carries a control character: %q", i, op.Why)
			}
		}
	})
}
