package logical

import (
	"reflect"
	"strings"
	"testing"
)

// The property Emit owes: what it writes, Parse reads back as the same
// operations. Without it a conversion of 242 diffs is a rewrite nobody can
// check.
func TestEmitRoundTrips(t *testing.T) {
	ops := []Op{
		{Why: "w", Path: Path{"dependencies", "openssl.org"}, Set: "^3"},
		{Why: "w", Path: Path{"build", "dependencies", "crates.io/semverator"}, Remove: true},
		{Why: "w", Path: Path{"build", "script"}, Substitute: true, From: "--path", To: "--locked --path"},
		{Why: "w", Path: Path{"build", "script"}, Append: []any{"make check"}},
		{Why: "w", Path: Path{"build", "script"}, Prepend: []any{"autoreconf -i"}},
		{Why: "w", Path: Path{"build", "env"}, Set: map[string]any{"ARGS": []any{"--prefix=x"}, "N": 14, "F": 1.5, "B": true}},
		{Why: "w", Path: Path{"a"}, Set: []any{}},
		{Why: "w", Path: Path{"b"}, Set: map[string]any{}},
		{Why: "w", Path: Path{"c"}, Set: nil},
		// A multi-line value, which has to become a heredoc: a quoted string
		// would render a build script as one unreadable line of \n.
		{Why: "w", Path: Path{"d"}, Set: "line one\nline two\n"},
		// And a heredoc INSIDE a list, where the separating comma cannot share
		// the marker's line.
		{Why: "w", Path: Path{"e"}, Set: []any{"first\nsecond\n", "plain"}},
		// A whole-list assignment carries what it believes it replaces.
		{Why: "w", Path: Path{"f"}, Set: []any{"new"}, Expect: []any{"old"}, HasExpect: true},
		// An edit whose own reason differs from the file's.
		{Why: "its own reason", Path: Path{"g"}, Set: "1"},
	}
	src := Emit(&Override{Project: "acme.org", Why: "w", Ops: ops})
	got, err := Parse(src, "t.hcl")
	if err != nil {
		t.Fatalf("Emit produced HCL that will not parse: %v\n%s", err, src)
	}
	if got.Project != "acme.org" || got.Why != "w" {
		t.Errorf("header lost: %+v", got)
	}
	if len(got.Ops) != len(ops) {
		t.Fatalf("got %d ops, want %d\n%s", len(got.Ops), len(ops), src)
	}
	for i := range ops {
		if !reflect.DeepEqual(got.Ops[i], ops[i]) {
			t.Errorf("ops[%d] round-tripped to something else:\n got %+v\nwant %+v", i, got.Ops[i], ops[i])
		}
	}
}

// HCL reads ${…} and %{…} inside a quoted string and inside a heredoc, and a
// recipe is full of $ARGS, ${PKGX_DIR:-$HOME/.pkgx} and 100%{_libdir}. They
// have to come back as themselves.
func TestEmitEscapesTemplateIntroducers(t *testing.T) {
	for _, v := range []string{
		`./configure ${PKGX_DIR:-$HOME/.pkgx} $ARGS`,
		"multi\n${LINE}\n100%{_libdir}\n",
		`%{x}`,
	} {
		src := Emit(&Override{Project: "p", Why: "w", Ops: []Op{{Why: "w", Path: Path{"a"}, Set: v}}})
		got, err := Parse(src, "t.hcl")
		if err != nil {
			t.Fatalf("%q: %v\n%s", v, err, src)
		}
		if got.Ops[0].Set != v {
			t.Errorf("%q came back as %q", v, got.Ops[0].Set)
		}
	}
}

// A value that CONTAINS the heredoc marker would close it early.
func TestEmitLengthensAMarkerTheTextContains(t *testing.T) {
	v := "before\nEOT\nafter\n"
	src := Emit(&Override{Project: "p", Why: "w", Ops: []Op{{Why: "w", Path: Path{"a"}, Set: v}}})
	got, err := Parse(src, "t.hcl")
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if got.Ops[0].Set != v {
		t.Errorf("got %q\n%s", got.Ops[0].Set, src)
	}
	// And a value that STARTS with the marker.
	v2 := "EOT\nrest\n"
	src2 := Emit(&Override{Project: "p", Why: "w", Ops: []Op{{Why: "w", Path: Path{"a"}, Set: v2}}})
	got2, err := Parse(src2, "t.hcl")
	if err != nil || got2.Ops[0].Set != v2 {
		t.Errorf("got %+v err %v\n%s", got2, err, src2)
	}
}

// A multi-line `why` becomes a heredoc too, and comes back whole.
func TestEmitAMultiLineWhy(t *testing.T) {
	why := "first line\nsecond line\n"
	src := Emit(&Override{Project: "p", Why: why, Ops: []Op{{Why: why, Path: Path{"a"}, Set: "1"}}})
	got, err := Parse(src, "t.hcl")
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if got.Why != why {
		t.Errorf("why = %q", got.Why)
	}
}

func TestEmitWithNoOps(t *testing.T) {
	src := Emit(&Override{Project: "p", Why: "w"})
	if strings.Contains(string(src), "edits") {
		t.Errorf("an override with nothing to do must not write an empty edits list:\n%s", src)
	}
}

// An unexpected Go type must not silently vanish from the output.
func TestEmitAnUnexpectedType(t *testing.T) {
	type odd struct{ A int }
	src := Emit(&Override{Project: "p", Why: "w", Ops: []Op{{Why: "w", Path: Path{"a"}, Set: odd{1}}}})
	if !strings.Contains(string(src), "{1}") {
		t.Errorf("got:\n%s", src)
	}
}

// `expect` refuses when what is there is neither what it replaces nor the
// result — which is the property a unified diff had and a blind assignment
// throws away.
func TestExpectRefusesAnUpstreamChange(t *testing.T) {
	op := Op{Why: "w", Path: Path{"build", "script"},
		Set: []any{"new"}, Expect: []any{"old"}, HasExpect: true}

	// It replaces what it expects.
	d := map[string]any{"build": map[string]any{"script": []any{"old"}}}
	if r, err := Apply(d, []Op{op}); err != nil || r[0].Outcome != Applied {
		t.Fatalf("%v %v", r, err)
	}
	// Run again: it is already the result. Redundant, not an error.
	if r, err := Apply(d, []Op{op}); err != nil || r[0].Outcome != Redundant {
		t.Fatalf("second time: %v %v", r, err)
	}
	// Somebody else changed it: refuse rather than throw their change away.
	d2 := map[string]any{"build": map[string]any{"script": []any{"someone else's"}}}
	r, err := Apply(d2, []Op{op})
	if err == nil || r[0].Outcome != PremiseGone {
		t.Fatalf("%v %v", r, err)
	}
	if !strings.Contains(err.Error(), "not what this replaces") {
		t.Errorf("error = %v", err)
	}
	// A key that is simply absent is not a contradiction: there is nothing to
	// throw away, so the assignment stands.
	d3 := map[string]any{"build": map[string]any{}}
	if r, err := Apply(d3, []Op{op}); err != nil || r[0].Outcome != Applied {
		t.Fatalf("absent: %v %v", r, err)
	}
}

// A heredoc value that does not end in a newline still has to close on a line
// that is exactly the marker.
func TestEmitAddsTheTrailingNewlineAHeredocNeeds(t *testing.T) {
	v := "one\ntwo" // no trailing newline
	src := Emit(&Override{Project: "p", Why: "w", Ops: []Op{{Why: "w", Path: Path{"a"}, Set: v}}})
	got, err := Parse(src, "t.hcl")
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	// HCL gives the body back WITH the newline the heredoc form requires; what
	// matters is that it parses and keeps both lines.
	s, _ := got.Ops[0].Set.(string)
	if !strings.HasPrefix(s, "one\ntwo") {
		t.Errorf("got %q", s)
	}
}

// int64 reaches the emitter from a document HCL itself produced, not only from
// YAML.
func TestEmitAnInt64(t *testing.T) {
	src := Emit(&Override{Project: "p", Why: "w", Ops: []Op{{Why: "w", Path: Path{"a"}, Set: int64(14)}}})
	got, err := Parse(src, "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	// Parse normalises HCL's int64 to the int the YAML reader would give, so
	// the two readers of one recipe agree about what they read.
	if got.Ops[0].Set != 14 {
		t.Errorf("got %#v", got.Ops[0].Set)
	}
}
