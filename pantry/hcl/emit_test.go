package hcl

import (
	"strings"
	"testing"
)

// roundTrips is the only assertion that matters: Convert refuses anything that
// does not parse back into the same recipe, so a call that returns without an
// error has already been checked.
func roundTrips(t *testing.T, yml string) string {
	t.Helper()
	out, err := Convert([]byte(yml))
	if err != nil {
		t.Fatalf("Convert refused:\n%s\n--- from ---\n%s", err, yml)
	}
	return string(out)
}

// TestConvertDefusesTemplateSyntax is the trap the whole conversion turns on.
// HCL reads ${…} as an interpolation, and recipe scripts are full of shell
// expansions. Left alone it either fails to parse or evaluates part of a build
// script.
func TestConvertDefusesTemplateSyntax(t *testing.T) {
	got := roundTrips(t, "build:\n  script: |\n    make PREFIX=${DEST} %{weird}\n")
	if !strings.Contains(got, "$${DEST}") || !strings.Contains(got, "%%{weird}") {
		t.Errorf("both sigils must be doubled:\n%s", got)
	}
}

// A heredoc body is written VERBATIM at column 0. The first version indented
// it with <<-, which strips the COMMON leading whitespace — so a fixture whose
// own code is indented came back with that indentation gone. Twenty-eight
// recipes were refused for it, and the refusal is why none shipped altered.
func TestConvertKeepsIndentationInsideAScript(t *testing.T) {
	yml := "test:\n  fixture: |\n    #include <stdio.h>\n    int main(void) {\n        return 0;\n    }\n"
	got := roundTrips(t, yml)
	if !strings.Contains(got, "\n    return 0;\n") {
		t.Errorf("the fixture's own indentation must survive:\n%s", got)
	}
}

// A string with no trailing newline cannot be a heredoc: the terminator
// implies one. Those are quoted instead, rather than silently gaining a byte.
func TestConvertQuotesAStringWithNoFinalNewline(t *testing.T) {
	got := roundTrips(t, "build:\n  script: \"a\\nb\"\n")
	if strings.Contains(got, "<<EOT") {
		t.Errorf("no heredoc for a string that does not end in a newline:\n%s", got)
	}
}

// A heredoc inside a LIST: the terminator must be alone on its line, so the
// comma cannot follow it. `EOT,` is where fifteen recipes died with
// "Unterminated template string".
func TestConvertHeredocInsideAList(t *testing.T) {
	roundTrips(t, "build:\n  script:\n    - |\n      one\n      two\n    - echo done\n")
}

// A map whose keys are not identifiers cannot be a block — HCL has no
// attribute called "openssl.org" — so it is an object attribute with quoted
// keys. One whose keys ARE identifiers reads better as a block.
func TestConvertChoosesBlocksAndObjects(t *testing.T) {
	got := roundTrips(t, "dependencies:\n  openssl.org: ^1.1\nbuild:\n  script: make\n")
	if !strings.Contains(got, `"openssl.org" = "^1.1"`) {
		t.Errorf("a dotted key must be a quoted object key:\n%s", got)
	}
	if !strings.Contains(got, "build {") {
		t.Errorf("an identifier-keyed map reads as a block:\n%s", got)
	}
}

// What Convert REFUSES is the point. HCL has one number type, so a YAML float
// that happens to be whole — MACOSX_DEPLOYMENT_TARGET: 11.0 — comes back as an
// int, and the information is absent from the format rather than lost by this
// code. Two of the overlay's 183 recipes are that, and they stay YAML.
func TestConvertRefusesWhatHCLCannotExpress(t *testing.T) {
	_, err := Convert([]byte("build:\n  env:\n    MACOSX_DEPLOYMENT_TARGET: 11.0\n  script: make\n"))
	if err == nil {
		t.Fatal("want a refusal for a whole float")
	}
	// And the message names the PATH and the TYPES: %#v renders int(11) and
	// float64(11) identically, so the first version of this error was unusable.
	for _, want := range []string{"MACOSX_DEPLOYMENT_TARGET", "float64", "int"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
}

func TestConvertRefusesBadInput(t *testing.T) {
	if _, err := Convert([]byte("provides: 123\n")); err == nil {
		t.Error("a recipe that fails the schema must not convert")
	}
	if _, err := Convert([]byte("\tnot: yaml\n")); err == nil {
		t.Error("undecodable yaml must not convert")
	}
}

// TestConvertEveryValueShape walks the shapes a recipe document can hold, in
// one recipe, so each arm of the renderer is exercised by something that also
// has to survive the round trip.
func TestConvertEveryValueShape(t *testing.T) {
	roundTrips(t, `
distributable:
  url: https://x/y.tar.gz
  strip-components: 1
dependencies:
  a.org: '*'
  b.org: 1
runtime:
  env:
    FLAG: true
    OFF: false
    EMPTY_LIST: []
    NUM: 3
    NESTED:
      deep.key: v
provides:
  - bin/x
build:
  script: make
platforms:
  - linux/x86-64
`)
}

// An empty map renders as {} rather than as a block with nothing in it, and an
// empty list as []. A block with an empty body parses, but it says "there is a
// build section" where the document says the section is empty.
func TestConvertEmptyContainers(t *testing.T) {
	got := roundTrips(t, "dependencies: {}\nprovides: []\nbuild:\n  script: make\n")
	if !strings.Contains(got, "dependencies = {}") || !strings.Contains(got, "provides = []") {
		t.Errorf("empty containers must stay empty and inline:\n%s", got)
	}
}

// A null survives as null: a recipe that says a key is deliberately empty is
// not the same as one that omits it.
func TestConvertNull(t *testing.T) {
	got := roundTrips(t, "dependencies:\n  a.org: ~\nbuild:\n  script: make\n")
	if !strings.Contains(got, "null") {
		t.Errorf("a null must render as null:\n%s", got)
	}
}

// ident decides between a bare name and a quoted key, and it is the thing
// standing between us and HCL that does not parse.
func TestIdent(t *testing.T) {
	for _, s := range []string{"build", "strip-components", "_x", "a1"} {
		if !ident(s) {
			t.Errorf("ident(%q) = false", s)
		}
	}
	for _, s := range []string{"", "openssl.org", "linux/x86-64", "1abc", "-lead", "a b"} {
		if ident(s) {
			t.Errorf("ident(%q) = true", s)
		}
	}
}

// firstDiff is what makes a refusal actionable. %#v renders int(1) and
// float64(1) identically, so the message has to carry the path and the types.
func TestFirstDiff(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want string
	}{
		{map[string]any{"k": 1}, map[string]any{"k": 1.0}, "[k]"},
		{map[string]any{"k": 1}, map[string]any{}, "present in the yaml"},
		{map[string]any{}, map[string]any{"k": 1}, "invented"},
		{[]any{1, 2}, []any{1}, "2 items from yaml, 1 from hcl"},
		{[]any{1, 2}, []any{1, 3}, "[1]"},
		{"a", "b", "."},
	} {
		got := firstDiff(c.a, c.b, "")
		if got == "" || !strings.Contains(got, c.want) {
			t.Errorf("firstDiff(%v,%v) = %q, want it to mention %q", c.a, c.b, got, c.want)
		}
	}
	// Equal things have no difference, and neither does a pair of nils.
	if d := firstDiff(map[string]any{"k": 1}, map[string]any{"k": 1}, ""); d != "" {
		t.Errorf("equal maps differ: %s", d)
	}
	if d := firstDiff(nil, nil, ""); d != "" {
		t.Errorf("two nils differ: %s", d)
	}
	if d := firstDiff(nil, 1, ""); d == "" {
		t.Error("nil against a value must differ")
	}
}

// quote's escapes, reached directly: a recipe carrying a tab, a backslash or a
// carriage return is rare enough that no fixture would hit them all, and a
// wrong escape here produces HCL that parses into a different string.
func TestQuoteEscapes(t *testing.T) {
	got := quote("a\"b\\c\nd\te\rf")
	want := `"a\"b\\c\nd\te\rf"`
	if got != want {
		t.Errorf("quote = %s, want %s", got, want)
	}
}

// expr's fallback. A YAML decode yields only the shapes above it, so this arm
// exists for a caller passing something else — and it must render SOMETHING
// rather than an empty expression, which would not parse at all.
func TestExprFallback(t *testing.T) {
	type odd struct{ A int }
	if got := expr(odd{1}, ""); !strings.HasPrefix(got, `"`) {
		t.Errorf("an unknown shape must still render as a string, got %s", got)
	}
}

// firstDiff walks a *pantry.Recipe, so it has to handle a pointer and a
// struct — which is how Convert calls it.
func TestFirstDiffOnRecipes(t *testing.T) {
	a, err := Parse([]byte(`dependencies = { "a.org" = "^1" }`+"\n"), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(`dependencies = { "a.org" = "^2" }`+"\n"), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	d := firstDiff(a, b, "")
	if !strings.Contains(d, "Dependencies") || !strings.Contains(d, "a.org") {
		t.Errorf("want the struct field and the map key named, got %q", d)
	}
	if firstDiff(a, a, "") != "" {
		t.Error("a recipe must not differ from itself")
	}
	// A nil pointer on one side is a difference, not a panic.
	var nilRec *struct{ X int }
	if firstDiff(nilRec, &struct{ X int }{1}, "") == "" {
		t.Error("nil against a value must differ")
	}
}

// A float that is not whole keeps its point: 1.5 is not 1, and the %g arm is
// the only thing that says so.
func TestConvertFractionalNumber(t *testing.T) {
	got := roundTrips(t, "build:\n  env:\n    X: 1.5\n  script: make\n")
	if !strings.Contains(got, "1.5") {
		t.Errorf("a fractional number must survive:\n%s", got)
	}
}

// firstDiff walks exported fields only. An unexported one cannot be read
// through reflection without panicking, and skipping it is not a gap: nothing
// a recipe carries lives there.
func TestFirstDiffSkipsUnexportedFields(t *testing.T) {
	type withHidden struct {
		Shown  int
		hidden int
	}
	a := withHidden{Shown: 1, hidden: 2}
	b := withHidden{Shown: 1, hidden: 3}
	if d := firstDiff(a, b, ""); d != "" {
		t.Errorf("an unexported field must not be compared, got %q", d)
	}
	if d := firstDiff(withHidden{Shown: 1}, withHidden{Shown: 2}, ""); d == "" {
		t.Error("an exported one must be")
	}
}

// A script containing a line that reads exactly EOT would end the heredoc
// early, and everything after it would be parsed as HCL. The terminator is
// chosen to avoid the body.
func TestConvertScriptContainingTheTerminator(t *testing.T) {
	got := roundTrips(t, "build:\n  script: |\n    cat <<EOT > f\n    hello\n    EOT\n    make\n")
	if !strings.Contains(got, "<<EOT_") {
		t.Errorf("the terminator must step aside for the body:\n%s", got)
	}
}

// Two equal slices are equal — the arm that says so was the last uncovered
// line, and a differ that only ever found differences would be useless.
func TestFirstDiffEqualSlices(t *testing.T) {
	if d := firstDiff([]any{1, "a"}, []any{1, "a"}, ""); d != "" {
		t.Errorf("equal slices differ: %s", d)
	}
}

// Convert must never hand back text it could not read. The guard cannot be
// reached through the public API while Emit is correct — which is the point of
// having it — so the seam stands in for the defect it exists to catch. It has
// caught one already: a heredoc whose terminator appeared in the script.
func TestConvertRefusesUnparseableOutput(t *testing.T) {
	old := emitFn
	t.Cleanup(func() { emitFn = old })
	emitFn = func(map[string]any) string { return "build { script = \n" }
	if _, err := Convert([]byte("build:\n  script: make\n")); err == nil ||
		!strings.Contains(err.Error(), "does not parse") {
		t.Errorf("want a refusal naming the parse failure, got %v", err)
	}
}
