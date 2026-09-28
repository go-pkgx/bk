package logical

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Path
	}{
		{"build", Path{"build"}},
		{"build.env.ARGS", Path{"build", "env", "ARGS"}},
		{`dependencies["openssl.org"]`, Path{"dependencies", "openssl.org"}},
		{`build.dependencies["crates.io/semverator"]`, Path{"build", "dependencies", "crates.io/semverator"}},
		{`a["b"].c`, Path{"a", "b", "c"}},
	} {
		got, err := ParsePath(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParsePath(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

// Every malformed path is refused at PARSE time, so a bad override cannot
// half-apply. A numeric index is refused on purpose: a position is the thing
// this package exists not to depend on.
func TestParsePathRefuses(t *testing.T) {
	for _, in := range []string{
		"",
		"a..b",
		"a.",
		"a[0]",
		`a["b`,
		`a["b"`,
		`a[""]`,
		"[b]",
	} {
		if p, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q) = %v, want an error", in, p)
		}
	}
}

func TestPathString(t *testing.T) {
	if got := (Path{"build", "dependencies", "crates.io/semverator"}).String(); got != `build.dependencies["crates.io/semverator"]` {
		t.Errorf("got %q", got)
	}
	if got := (Path{"a", "b"}).String(); got != "a.b" {
		t.Errorf("got %q", got)
	}
}

func TestOutcomeString(t *testing.T) {
	for o, want := range map[Outcome]string{Applied: "applied", Redundant: "redundant", PremiseGone: "premise gone"} {
		if got := o.String(); got != want {
			t.Errorf("%d = %q, want %q", o, got, want)
		}
	}
}

func doc() map[string]any {
	return map[string]any{
		"dependencies": map[string]any{"openssl.org": "^1.1"},
		"build": map[string]any{
			"script": []any{"./configure $ARGS", "make install"},
			"env":    map[string]any{"ARGS": []any{"--prefix=x"}},
		},
	}
}

func TestSetAddsMergesAndReportsRedundant(t *testing.T) {
	d := doc()
	p, _ := ParsePath(`dependencies["zlib.net"]`)
	r, err := Apply(d, []Op{{Why: "w", Path: p, Set: "^1"}})
	if err != nil || r[0].Outcome != Applied {
		t.Fatalf("%v %v", r, err)
	}
	// Applying it again is redundant, not an error: an override has to be
	// safe to run twice or it cannot be a gate.
	r, err = Apply(d, []Op{{Why: "w", Path: p, Set: "^1"}})
	if err != nil || r[0].Outcome != Redundant {
		t.Fatalf("second time: %v %v", r, err)
	}
	// A map value DEEP-MERGES rather than replacing the block.
	p2, _ := ParsePath("dependencies")
	if _, err := Apply(d, []Op{{Why: "w", Path: p2, Set: map[string]any{"curl.se": "^8"}}}); err != nil {
		t.Fatal(err)
	}
	deps := d["dependencies"].(map[string]any)
	if deps["openssl.org"] != "^1.1" || deps["curl.se"] != "^8" {
		t.Errorf("merge lost a sibling: %v", deps)
	}
	// A missing parent is created for a set, and only for a set.
	p3, _ := ParsePath("test.dependencies.x")
	if _, err := Apply(d, []Op{{Why: "w", Path: p3, Set: "1"}}); err != nil {
		t.Fatal(err)
	}
}

func TestSetRefusesToWalkThroughAScalar(t *testing.T) {
	d := doc()
	p, _ := ParsePath(`dependencies["openssl.org"].x`)
	r, err := Apply(d, []Op{{Why: "w", Path: p, Set: "1"}})
	if err == nil || r[0].Outcome != PremiseGone {
		t.Fatalf("%v %v", r, err)
	}
	if !strings.Contains(err.Error(), "not a mapping") {
		t.Errorf("error should name the shape: %v", err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	d := doc()
	p, _ := ParsePath(`dependencies["openssl.org"]`)
	r, _ := Apply(d, []Op{{Why: "w", Path: p, Remove: true}})
	if r[0].Outcome != Applied {
		t.Fatalf("%v", r)
	}
	r, err := Apply(d, []Op{{Why: "w", Path: p, Remove: true}})
	if err != nil || r[0].Outcome != Redundant {
		t.Fatalf("%v %v", r, err)
	}
	// A parent that is not there is also nothing to do, not a failure.
	p2, _ := ParsePath("absent.thing")
	r, err = Apply(d, []Op{{Why: "w", Path: p2, Remove: true}})
	if err != nil || r[0].Outcome != Redundant {
		t.Fatalf("%v %v", r, err)
	}
}

func TestAppendAndPrepend(t *testing.T) {
	d := doc()
	p, _ := ParsePath("build.script")
	r, err := Apply(d, []Op{{Why: "w", Path: p, Append: []any{"make check"}}})
	if err != nil || r[0].Outcome != Applied {
		t.Fatalf("%v %v", r, err)
	}
	if got := d["build"].(map[string]any)["script"].([]any); got[len(got)-1] != "make check" {
		t.Errorf("got %v", got)
	}
	// Twice is redundant: matched by VALUE, so a rerun does not double it.
	r, _ = Apply(d, []Op{{Why: "w", Path: p, Append: []any{"make check"}}})
	if r[0].Outcome != Redundant {
		t.Errorf("%v", r)
	}
	r, _ = Apply(d, []Op{{Why: "w", Path: p, Prepend: []any{"autoreconf -i"}}})
	if r[0].Outcome != Applied || d["build"].(map[string]any)["script"].([]any)[0] != "autoreconf -i" {
		t.Errorf("%v", d["build"])
	}
}

func TestAppendRefusesWhatIsNotAList(t *testing.T) {
	d := doc()
	for _, path := range []string{"build.env", "build.absent", "absent.x"} {
		p, _ := ParsePath(path)
		if _, err := Apply(d, []Op{{Why: "w", Path: p, Append: []any{"x"}}}); err == nil {
			t.Errorf("%s: appending to it must fail", path)
		}
	}
}

// The three outcomes, which is the whole reason for this format.
func TestSubstituteTellsTheThreeCasesApart(t *testing.T) {
	d := doc()
	p, _ := ParsePath("build.script")
	r, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "make install", To: "make -j4 install"}})
	if err != nil || r[0].Outcome != Applied {
		t.Fatalf("applied: %v %v", r, err)
	}
	// Upstream has caught up: the new text is there, the old is not. Redundant,
	// and an override reported this way should be DELETED, not carried.
	r, err = Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "make install", To: "make -j4 install"}})
	if err != nil || r[0].Outcome != Redundant {
		t.Fatalf("redundant: %v %v", r, err)
	}
	// Neither: what this edits is gone. An error, because silently not
	// applying is how a -Werror switch went missing for weeks.
	r, err = Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "cmake", To: "cmake3"}})
	if err == nil || r[0].Outcome != PremiseGone {
		t.Fatalf("premise gone: %v %v", r, err)
	}
}

func TestSubstituteOnAStringAndAStep(t *testing.T) {
	d := map[string]any{
		"build": map[string]any{
			"script": []any{
				map[string]any{"run": "cargo install --path .", "if": ">=1.0"},
				7, // not text, and not a mapping: passed through untouched
			},
			"working-directory": "src",
		},
	}
	p, _ := ParsePath("build.script")
	if _, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "--path", To: "--locked --path"}}); err != nil {
		t.Fatal(err)
	}
	step := d["build"].(map[string]any)["script"].([]any)[0].(map[string]any)
	if step["run"] != "cargo install --locked --path ." || step["if"] != ">=1.0" {
		t.Errorf("got %v", step)
	}
	// A plain string value works the same way.
	p2, _ := ParsePath("build.working-directory")
	if _, err := Apply(d, []Op{{Why: "w", Path: p2, Substitute: true, From: "src", To: "build"}}); err != nil {
		t.Fatal(err)
	}
	if d["build"].(map[string]any)["working-directory"] != "build" {
		t.Errorf("got %v", d["build"])
	}
	// Redundant on a plain string too.
	r, err := Apply(d, []Op{{Why: "w", Path: p2, Substitute: true, From: "src", To: "build"}})
	if err != nil || r[0].Outcome != Redundant {
		t.Errorf("%v %v", r, err)
	}
}

func TestSubstituteRefuses(t *testing.T) {
	d := doc()
	for _, tc := range []struct{ name, path, from string }{
		{"empty from would match everywhere", "build.script", ""},
		{"missing parent", "absent.x", "a"},
		{"missing key", "build.absent", "a"},
		{"not text", "build.env", "a"},
	} {
		p, _ := ParsePath(tc.path)
		if _, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: tc.from, To: "z"}}); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	// A list that simply does not contain it.
	p, _ := ParsePath("build.script")
	if _, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "nowhere", To: "z"}}); err == nil {
		t.Error("a run no element holds must fail")
	}
}

// Apply stops at the first premise that is gone: a half-applied override is a
// recipe nobody wrote.
func TestApplyStopsAtTheFirstFailure(t *testing.T) {
	d := doc()
	bad, _ := ParsePath("build.script")
	after, _ := ParsePath(`dependencies["zlib.net"]`)
	res, err := Apply(d, []Op{
		{Why: "w", Path: bad, Substitute: true, From: "nowhere", To: "z"},
		{Why: "w", Path: after, Set: "^1"},
	})
	if err == nil || len(res) != 1 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if _, had := d["dependencies"].(map[string]any)["zlib.net"]; had {
		t.Error("the operation after the failure must not have run")
	}
}
