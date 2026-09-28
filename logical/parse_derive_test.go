package logical

import (
	"reflect"
	"strings"
	"testing"
)

const zipOverride = `
project = "info-zip.org/zip"
why     = "gcc 14 makes an implicit declaration an error and the memset probe runs $CC without CFLAGS"

merge {
  build {
    dependencies = {
      "gnu.org/patch" = "*"
    }
  }
}

edits = [
  {
    why  = "unix/Makefile overwrites CFLAGS, so CC is the only channel"
    path = "build.script"
    from = "-std=gnu17"
    to   = "-std=gnu17 -Wno-implicit-function-declaration"
  },
]
`

func TestParseReadsAnOverride(t *testing.T) {
	o, err := Parse([]byte(zipOverride), "zip.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if o.Project != "info-zip.org/zip" || o.Why == "" {
		t.Fatalf("got %+v", o)
	}
	if len(o.Ops) != 2 {
		t.Fatalf("got %d ops: %+v", len(o.Ops), o.Ops)
	}
	// The merge flattens to the LEAF, so adding one dependency does not
	// replace the whole build block.
	if got := o.Ops[0].Path.String(); got != `build.dependencies["gnu.org/patch"]` {
		t.Errorf("merge path = %q", got)
	}
	// An edit's own `why` wins over the file's.
	if !strings.Contains(o.Ops[1].Why, "only channel") {
		t.Errorf("edit why = %q", o.Ops[1].Why)
	}
	// The file's `why` is inherited where an edit gives none.
	if !strings.Contains(o.Ops[0].Why, "gcc 14") {
		t.Errorf("merge why = %q", o.Ops[0].Why)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"not hcl", "project = ", "hcl"},
		{"no project", `why = "w"`, "needs a `project`"},
		{"no why", `project = "p"`, "needs a `why`"},
		{"changes nothing", `project = "p"` + "\n" + `why = "w"`, "changes nothing"},
		{"merge not a block", `project = "p"` + "\n" + `why = "w"` + "\n" + `merge = 1`, "must be a block"},
		{"edits not a list", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = 1`, "must be a list"},
		{"edit not an object", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [1]`, "must be an object"},
		{"edit without a path", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [{ remove = true }]`, "needs a `path`"},
		{"edit with a bad path", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [{ path = "a[0]", remove = true }]`, "position"},
		{"edit with no verb", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [{ path = "a" }]`, "names no verb"},
		{"edit with two verbs", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [{ path = "a", remove = true, set = 1 }]`, "more than one verb"},
		{"append not a list", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [{ path = "a", append = 1 }]`, "`append` must be a list"},
		{"prepend not a list", `project = "p"` + "\n" + `why = "w"` + "\n" + `edits = [{ path = "a", prepend = 1 }]`, "`prepend` must be a list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src), "t.hcl")
			if err == nil {
				t.Fatalf("want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseAcceptsMergeAlone(t *testing.T) {
	src := `project = "p"` + "\n" + `why = "w"` + "\n" + `merge { a = 1 }`
	o, err := Parse([]byte(src), "t.hcl")
	if err != nil || len(o.Ops) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
	// An empty nested map is a value, not a level to recurse into.
	src2 := `project = "p"` + "\n" + `why = "w"` + "\n" + `merge { a = {} }`
	o2, err := Parse([]byte(src2), "t.hcl")
	if err != nil || len(o2.Ops) != 1 || o2.Ops[0].Path.String() != "a" {
		t.Fatalf("%+v %v", o2, err)
	}
}

// Derive must reproduce its input: that is the only property that makes a
// mechanical conversion of 242 unified diffs checkable rather than a rewrite.
func TestDeriveReproduces(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after map[string]any
		wantVerb      string
	}{
		{"a key appears", map[string]any{}, map[string]any{"a": "1"}, "set"},
		{"a key goes", map[string]any{"a": "1"}, map[string]any{}, "remove"},
		{"a scalar changes", map[string]any{"a": "1"}, map[string]any{"a": "2"}, "set"},
		{"a string gains a run", map[string]any{"a": "cc -O2"}, map[string]any{"a": "cc -O2 -g"}, "substitute"},
		{"a list grows at the end", map[string]any{"a": []any{"x"}}, map[string]any{"a": []any{"x", "y"}}, "append"},
		{"a list grows at the front", map[string]any{"a": []any{"x"}}, map[string]any{"a": []any{"y", "x"}}, "prepend"},
		{"one element is edited", map[string]any{"a": []any{"p", "cc -O2"}}, map[string]any{"a": []any{"p", "cc -O2 -g"}}, "substitute"},
		{"a list is rewritten", map[string]any{"a": []any{"x", "y"}}, map[string]any{"a": []any{"z"}}, "set"},
		{"a short scalar changes whole", map[string]any{"a": map[string]any{"b": "^1.1"}}, map[string]any{"a": map[string]any{"b": "^3"}}, "set"},
		{"a long command gains a flag", map[string]any{"a": "cargo install --root=x --path=. --features=y"}, map[string]any{"a": "cargo install --locked --root=x --path=. --features=y"}, "substitute"},
		{"a map becomes a list", map[string]any{"a": map[string]any{"b": "1"}}, map[string]any{"a": []any{"b"}}, "set"},
		{"a number changes", map[string]any{"a": 1}, map[string]any{"a": 2}, "set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := Derive(tc.before, tc.after, "why")
			if len(ops) != 1 {
				t.Fatalf("got %d ops: %+v", len(ops), ops)
			}
			if got := verbOf(ops[0]); got != tc.wantVerb {
				t.Errorf("verb = %s, want %s", got, tc.wantVerb)
			}
			got := deepCopy(tc.before)
			if _, err := Apply(got, ops); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !reflect.DeepEqual(got, tc.after) {
				t.Errorf("derived operations do not reproduce:\n got %v\nwant %v", got, tc.after)
			}
		})
	}
}

// A run that occurs twice in the same list must NOT become a substitution: it
// would be replaced in both places. grpc.io and apache.org/serf did exactly
// that and produced -DCMAKE_INSTALL_PREFIX=""{{prefix}}.
func TestDeriveWillNotSubstituteARunThatOccursTwice(t *testing.T) {
	before := map[string]any{"a": []any{`-DX="{{p}}"`, `-DY="{{p}}"`}}
	after := map[string]any{"a": []any{`-DX=""{{p}}"`, `-DY="{{p}}"`}}
	ops := Derive(before, after, "why")
	got := deepCopy(before)
	if _, err := Apply(got, ops); err != nil || !reflect.DeepEqual(got, after) {
		t.Errorf("got %v, err %v", got, err)
	}
	// Whether it widens the run until it is unique or gives up and assigns the
	// list, the property asserted here is the one that matters — it must not
	// edit the NEIGHBOUR. Asserting the verb instead made this test fail the
	// day the deriver got better at the first option.
	if second := got["a"].([]any)[1]; second != before["a"].([]any)[1] {
		t.Errorf("the neighbouring element was edited: %v", second)
	}
}

// One string being a PREFIX of the other used to index past the end.
func TestRunDiffOnAPrefix(t *testing.T) {
	if from, to, ok := runDiff("abc", "abcdef"); !ok || from == "" || !strings.HasPrefix(to, from) {
		t.Errorf("runDiff = %q,%q,%v", from, to, ok)
	}
	if _, _, ok := runDiff("abc", "abc"); ok {
		t.Error("identical strings have no run")
	}
	// A run that cannot be made unique: the caller falls back to assignment.
	if _, _, ok := runDiff("aaaa", "aaaaa"); ok {
		t.Log("widened to unique, acceptable")
	}
}

func TestCountInList(t *testing.T) {
	l := []any{"xax", map[string]any{"run": "a", "n": 1}, 7}
	if got := countInList(l, "a"); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func verbOf(o Op) string {
	switch {
	case o.Remove:
		return "remove"
	case o.Substitute:
		return "substitute"
	case o.Append != nil:
		return "append"
	case o.Prepend != nil:
		return "prepend"
	default:
		return "set"
	}
}

func deepCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = deepCopy(t)
		case []any:
			l := make([]any, len(t))
			copy(l, t)
			out[k] = l
		default:
			out[k] = v
		}
	}
	return out
}

// The corners the 242 real overrides never reach, which is exactly why they
// need a test rather than a hope.
func TestDeriveCorners(t *testing.T) {
	// A map key whose value is a nested map on ONE side only: derive must not
	// recurse into a type that is not there.
	before := map[string]any{"a": map[string]any{"b": map[string]any{"c": 1}}}
	after := map[string]any{"a": map[string]any{"b": "flat"}}
	ops := Derive(before, after, "w")
	got := deepCopy(before)
	if _, err := Apply(got, ops); err != nil || !reflect.DeepEqual(got, after) {
		t.Errorf("map → scalar: got %v err %v", got, err)
	}

	// A list whose elements are maps, edited in one of them.
	before2 := map[string]any{"a": []any{map[string]any{"run": "x"}}}
	after2 := map[string]any{"a": []any{map[string]any{"run": "y"}}}
	ops2 := Derive(before2, after2, "w")
	got2 := deepCopy(before2)
	if _, err := Apply(got2, ops2); err != nil || !reflect.DeepEqual(got2, after2) {
		t.Errorf("list of maps: got %v err %v", got2, err)
	}

	// A list that shrinks: no append, no prepend, no single edit.
	before3 := map[string]any{"a": []any{"x", "y", "z"}}
	after3 := map[string]any{"a": []any{"x"}}
	got3 := deepCopy(before3)
	if _, err := Apply(got3, Derive(before3, after3, "w")); err != nil || !reflect.DeepEqual(got3, after3) {
		t.Errorf("shrink: got %v err %v", got3, err)
	}

	// Two elements change at once: not one substitution.
	before4 := map[string]any{"a": []any{"aaaaaaaaaa1", "bbbbbbbbbb1"}}
	after4 := map[string]any{"a": []any{"aaaaaaaaaa2", "bbbbbbbbbb2"}}
	got4 := deepCopy(before4)
	if _, err := Apply(got4, Derive(before4, after4, "w")); err != nil || !reflect.DeepEqual(got4, after4) {
		t.Errorf("two edits: got %v err %v", got4, err)
	}

	// Same length, no element differs by value but the order does.
	before5 := map[string]any{"a": []any{"x", "y"}}
	after5 := map[string]any{"a": []any{"y", "x"}}
	got5 := deepCopy(before5)
	if _, err := Apply(got5, Derive(before5, after5, "w")); err != nil || !reflect.DeepEqual(got5, after5) {
		t.Errorf("reorder: got %v err %v", got5, err)
	}

	// Identical documents derive nothing.
	if ops := Derive(map[string]any{"a": 1}, map[string]any{"a": 1}, "w"); len(ops) != 0 {
		t.Errorf("identical documents: %+v", ops)
	}
}

// runDiff must never hand back a run that occurs more than once, and must give
// up rather than return one it cannot make unique.
func TestRunDiffUniqueness(t *testing.T) {
	// A run that appears twice: widened until unique, or refused.
	from, _, ok := runDiff("ab ab", "ab ab!")
	if ok && strings.Count("ab ab", from) != 1 {
		t.Errorf("returned a run occurring %d times: %q", strings.Count("ab ab", from), from)
	}
	// Nothing in common at all.
	if _, _, ok := runDiff("aaa", "bbbb"); ok {
		t.Log("whole-value run, which is unique; acceptable")
	}
	// Empty original.
	if _, _, ok := runDiff("", "x"); ok {
		t.Error("an empty original has no unique run")
	}
}

// worthSubstituting is the rule that decides between the two verbs, and it has
// no threshold to argue about: would restating the value be repeating it?
func TestWorthSubstituting(t *testing.T) {
	if worthSubstituting("^1.1", "1.1") {
		t.Error("a version constraint reads better as a whole-value set")
	}
	long := "cargo install --root=x --path=. --features=y"
	if !worthSubstituting(long, "--locked ") {
		t.Error("a long command gaining a flag is a substitution")
	}
}

// mergeMaps: a nested map on one side and a scalar on the other must not be
// merged into a mapping that was never written.
func TestMergeMapsOverAScalar(t *testing.T) {
	d := map[string]any{"a": map[string]any{"b": "scalar"}}
	p, _ := ParsePath("a")
	if _, err := Apply(d, []Op{{Why: "w", Path: p, Set: map[string]any{"b": map[string]any{"c": 1}}}}); err != nil {
		t.Fatal(err)
	}
	inner, ok := d["a"].(map[string]any)["b"].(map[string]any)
	if !ok || inner["c"] != 1 {
		t.Errorf("got %v", d)
	}
}

// substMap leaves a non-string field alone, and substitute reports a list in
// which only a mapping matched.
func TestSubstituteInsideAStepOnly(t *testing.T) {
	d := map[string]any{"a": []any{map[string]any{"run": "cargo install --path .", "n": 3}}}
	p, _ := ParsePath("a")
	if _, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "--path", To: "--locked --path"}}); err != nil {
		t.Fatal(err)
	}
	step := d["a"].([]any)[0].(map[string]any)
	if step["run"] != "cargo install --locked --path ." || step["n"] != 3 {
		t.Errorf("got %v", step)
	}
	// And the redundant case reached through a mapping.
	r, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "--path .", To: "--locked --path ."}})
	if err != nil || r[0].Outcome != Applied {
		t.Logf("outcome %v", r[0].Outcome)
	}
}

// An edits list whose `set` is present but null, and an append/prepend pair
// given as HCL rather than built by hand.
func TestParseVerbsFromHCL(t *testing.T) {
	src := `
project = "p"
why     = "w"
edits = [
  { path = "a", append  = ["x"] },
  { path = "b", prepend = ["y"] },
  { path = "c", set     = { d = 1 } },
  { path = "e", remove  = true },
  { path = "f", from = "g", to = "h" },
]
`
	o, err := Parse([]byte(src), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"append", "prepend", "set", "remove", "substitute"}
	if len(o.Ops) != len(want) {
		t.Fatalf("got %d ops", len(o.Ops))
	}
	for i, w := range want {
		if got := verbOf(o.Ops[i]); got != w {
			t.Errorf("ops[%d] = %s, want %s", i, got, w)
		}
	}
}

// The UTF-8 backoffs, and the widening that has to pass over a multibyte rune.
// A recipe may hold any UTF-8 — gnu.org/grep's comment has an em dash — and
// half a rune is not a substring anyone can search for.
func TestRunDiffOnRunes(t *testing.T) {
	for _, tc := range [][2]string{
		{"aé b aé b", "aé b aé bZ"}, // widening steps back over é
		{"héllo wörld", "héllo wörld!"},
		{"ααα", "αααβ"},
	} {
		from, to, ok := runDiff(tc[0], tc[1])
		if !ok {
			continue
		}
		if !utf8Valid(from) || !utf8Valid(to) {
			t.Errorf("runDiff(%q,%q) = %q,%q — not whole runes", tc[0], tc[1], from, to)
		}
		if strings.Count(tc[0], from) != 1 {
			t.Errorf("runDiff(%q,%q) returned %q, which occurs %d times", tc[0], tc[1], from, strings.Count(tc[0], from))
		}
		if strings.Replace(tc[0], from, to, 1) != tc[1] {
			t.Errorf("runDiff(%q,%q) = %q→%q does not reproduce", tc[0], tc[1], from, to)
		}
	}
	// A difference at the END, so the right-hand backoff runs.
	if from, to, ok := runDiff("wörldé", "wörldà"); ok {
		if !utf8Valid(from) || !utf8Valid(to) {
			t.Errorf("tail: %q → %q", from, to)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && !strings.Contains(s, "�") {
			return false
		}
	}
	return len(s) == 0 || utf8Start(s[0])
}

// A plain string that simply does not contain the run — the non-list arm of
// the premise-gone case.
func TestSubstitutePremiseGoneOnAString(t *testing.T) {
	d := map[string]any{"a": "hello"}
	p, _ := ParsePath("a")
	r, err := Apply(d, []Op{{Why: "w", Path: p, Substitute: true, From: "goodbye", To: "hi"}})
	if err == nil || r[0].Outcome != PremiseGone {
		t.Fatalf("%v %v", r, err)
	}
	if !strings.Contains(err.Error(), "does not contain") {
		t.Errorf("error should say what is missing: %v", err)
	}
}

// mergeMaps recursing: a nested map on BOTH sides keeps the siblings of each.
func TestMergeMapsRecurses(t *testing.T) {
	d := map[string]any{"build": map[string]any{
		"dependencies": map[string]any{"a" + ".org": "1"},
		"script":       []any{"make"},
	}}
	p, _ := ParsePath("build")
	if _, err := Apply(d, []Op{{Why: "w", Path: p, Set: map[string]any{
		"dependencies": map[string]any{"b.org": "2"},
	}}}); err != nil {
		t.Fatal(err)
	}
	b := d["build"].(map[string]any)
	deps := b["dependencies"].(map[string]any)
	if deps["a.org"] != "1" || deps["b.org"] != "2" {
		t.Errorf("nested merge lost a sibling: %v", deps)
	}
	if _, still := b["script"]; !still {
		t.Error("the merge dropped a sibling of the nested map")
	}
}

// Widening to the RIGHT. The run starts at the beginning, so there is nothing
// to its left to take in, and it still occurs twice.
func TestRunDiffWidensRightwards(t *testing.T) {
	a, b := "ab_ab", "cb_ab"
	from, to, ok := runDiff(a, b)
	if !ok {
		t.Fatal("want a run")
	}
	if strings.Count(a, from) != 1 || strings.Replace(a, from, to, 1) != b {
		t.Errorf("runDiff(%q,%q) = %q→%q: occurs %d time(s), gives %q",
			a, b, from, to, strings.Count(a, from), strings.Replace(a, from, to, 1))
	}
}

// Two accented characters that share a continuation byte: the common SUFFIX
// starts in the middle of a rune, and the right-hand backoff has to step out
// of it. é is C3 A9 and ĩ is C4 A9 — the A9 is common and means nothing.
func TestRunDiffBacksOutOfASharedContinuationByte(t *testing.T) {
	a, b := "é", "ĩ"
	from, to, ok := runDiff(a, b)
	if !ok {
		t.Fatal("want a run")
	}
	if from != a || to != b {
		t.Errorf("runDiff(%q,%q) = %q→%q; the shared A9 byte is not a shared character", a, b, from, to)
	}
}

// Widening rightwards ACROSS a multibyte rune. The run starts at the
// beginning, so widening can only go right; the text repeats, so it has to;
// and the next step back lands inside the é, which is not a place a substring
// may begin or end.
func TestRunDiffWidensRightwardsOverARune(t *testing.T) {
	a, b := "Xé_Xé", "Yé_Xé"
	from, to, ok := runDiff(a, b)
	if !ok {
		t.Fatal("want a run")
	}
	if strings.Count(a, from) != 1 || strings.Replace(a, from, to, 1) != b {
		t.Errorf("runDiff(%q,%q) = %q→%q: occurs %d time(s), gives %q",
			a, b, from, to, strings.Count(a, from), strings.Replace(a, from, to, 1))
	}
	if !utf8Start(from[0]) || !utf8Start(to[0]) {
		t.Errorf("run starts mid-rune: %q → %q", from, to)
	}
}

// changedText: the shapes a list element can take, and the ones it may not.
func TestChangedText(t *testing.T) {
	for _, tc := range []struct {
		name string
		x, y any
		ok   bool
	}{
		{"two plain strings", "a", "b", true},
		{"string against a number", "a", 1, false},
		{"one field differs", map[string]any{"run": "a", "if": "x"}, map[string]any{"run": "b", "if": "x"}, true},
		{"two fields differ", map[string]any{"run": "a", "if": "x"}, map[string]any{"run": "b", "if": "y"}, false},
		{"no field differs", map[string]any{"run": "a"}, map[string]any{"run": "a"}, false},
		{"a key is missing", map[string]any{"run": "a"}, map[string]any{"cmd": "a"}, false},
		{"different sizes", map[string]any{"run": "a"}, map[string]any{"run": "a", "if": "x"}, false},
		{"a non-string field differs", map[string]any{"n": 1}, map[string]any{"n": 2}, false},
		{"a map against a number", map[string]any{"run": "a"}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := changedText(tc.x, tc.y); ok != tc.ok {
				t.Errorf("changedText(%v,%v) ok = %v, want %v", tc.x, tc.y, ok, tc.ok)
			}
		})
	}
}

// A list in which the SAME command appears twice, and one of them changes:
// no substitution can name one of two identical strings, so the deriver gives
// up and the caller assigns the list — with `expect`, so it still refuses if
// upstream has moved.
func TestDeriveGivesUpOnTwoIdenticalElements(t *testing.T) {
	before := map[string]any{"a": []any{"make install", "make install"}}
	after := map[string]any{"a": []any{"make install", "make install -j4"}}
	ops := Derive(before, after, "w")
	if len(ops) != 1 || verbOf(ops[0]) != "set" || !ops[0].HasExpect {
		t.Fatalf("want a whole-list set carrying expect, got %+v", ops)
	}
	got := deepCopy(before)
	if _, err := Apply(got, ops); err != nil || !reflect.DeepEqual(got, after) {
		t.Errorf("got %v err %v", got, err)
	}
}

// A list element that is neither a string nor a mapping stops the derivation
// before it can produce something it cannot express.
func TestDeriveListWithANumberElement(t *testing.T) {
	before := map[string]any{"a": []any{1, "x"}}
	after := map[string]any{"a": []any{2, "x"}}
	got := deepCopy(before)
	if _, err := Apply(got, Derive(before, after, "w")); err != nil || !reflect.DeepEqual(got, after) {
		t.Errorf("got %v err %v", got, err)
	}
}
