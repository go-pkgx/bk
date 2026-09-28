package logical

import (
	"reflect"
	"sort"
	"strings"
)

// Derive works out the operations that turn `before` into `after`.
//
// It exists so that 242 unified diffs can be converted without anyone retyping
// them, and — more importantly — so the conversion can be CHECKED: apply what
// Derive returns to `before` and the result must equal `after`, document for
// document. A migration nobody can check is a rewrite.
//
// Two rules matter and both come from the same place, that a position is not
// an identity:
//
//   - list elements are matched by VALUE, never by index;
//   - when a list differs by exactly one element out and one element in, and
//     those two are the same string with one contiguous run changed, it is
//     emitted as a SUBSTITUTION rather than a whole-list assignment. That is
//     the "append a flag to this command" shape, 32 of the 42 hard cases, and
//     writing the list out instead would fork it.
func Derive(before, after map[string]any, why string) []Op {
	var ops []Op
	derive(before, after, nil, why, &ops)
	return ops
}

func derive(a, b any, path Path, why string, out *[]Op) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			x, in1 := am[k]
			y, in2 := bm[k]
			p := append(append(Path{}, path...), k)
			switch {
			case in1 && !in2:
				*out = append(*out, Op{Why: why, Path: p, Remove: true})
			case !in1 && in2:
				*out = append(*out, Op{Why: why, Path: p, Set: y})
			default:
				derive(x, y, p, why, out)
			}
		}
		return
	}
	if reflect.DeepEqual(a, b) {
		return
	}
	al, aok2 := a.([]any)
	bl, bok2 := b.([]any)
	if aok2 && bok2 {
		if ops, ok := deriveList(al, bl, path, why); ok {
			*out = append(*out, ops...)
			return
		}
		// Replacing a whole list that was already there is the one operation
		// that can swallow an upstream change without saying so — a diff would
		// at least have refused. So it carries what it believes it is
		// replacing, and refuses too.
		*out = append(*out, Op{Why: why, Path: path, Set: b, Expect: a, HasExpect: true})
		return
	}
	if as, ok := a.(string); ok {
		if bs, ok2 := b.(string); ok2 {
			if from, to, ok3 := runDiff(as, bs); ok3 && worthSubstituting(as, from) {
				*out = append(*out, Op{Why: why, Path: path, Substitute: true, From: from, To: to})
				return
			}
		}
	}
	*out = append(*out, Op{Why: why, Path: path, Set: b})
}

// deriveList prefers append/prepend/substitute over restating the list.
//
// Several elements may each be a substitution, and then several substitutions
// is the right answer rather than one whole-list assignment:
// info-zip.org/zip's override adds `.patch` to ten filenames, bumps a URL and
// extends a compiler line. As one assignment that is a fork of a 13-line
// script which would silently survive upstream adding a fourteenth; as twelve
// substitutions each one says what it does and each one refuses if its target
// has moved.
func deriveList(a, b []any, path Path, why string) ([]Op, bool) {
	// Pure append: a is a prefix of b.
	if len(b) > len(a) && reflect.DeepEqual(a, b[:len(a)]) {
		return []Op{{Why: why, Path: path, Append: append([]any{}, b[len(a):]...)}}, true
	}
	// Pure prepend: a is a suffix of b.
	if len(b) > len(a) && reflect.DeepEqual(a, b[len(b)-len(a):]) {
		return []Op{{Why: why, Path: path, Prepend: append([]any{}, b[:len(b)-len(a)]...)}}, true
	}
	if len(a) != len(b) {
		return nil, false
	}
	var ops []Op
	for i := range a {
		if reflect.DeepEqual(a[i], b[i]) {
			continue
		}
		as, bs, ok := changedText(a[i], b[i])
		if !ok {
			return nil, false
		}
		from, to, ok := runDiffIn(as, bs, func(run string) int { return countInList(a, run) })
		if !ok || !worthSubstituting(as, from) {
			// Unique in the WHOLE list, not merely in the element that
			// changed: substitute walks every element, so a run that also
			// occurs in a neighbour would be replaced there too. grpc.io and
			// apache.org/serf both did exactly that, and produced
			// -DCMAKE_INSTALL_PREFIX=""{{prefix}} — found by demanding the
			// derived operations reproduce the diff, not by reading this.
			return nil, false
		}
		ops = append(ops, Op{Why: why, Path: path, Substitute: true, From: from, To: to})
	}
	// No `len(ops) == 0` guard: derive only reaches here for two lists that
	// are NOT DeepEqual, and two lists of the same length whose elements are
	// all DeepEqual are DeepEqual. The loop therefore always produced at least
	// one operation. A guard for it would have been untestable, which is the
	// tell.
	return ops, true
}

// runDiff finds the single contiguous run by which two strings differ, and
// returns it as a from/to pair that is UNIQUE in the original — a substitution
// that matched twice would edit somewhere nobody looked.
//
// It refuses a run so short it would match by accident; the caller then falls
// back to assigning the whole value, which is always correct and merely more
// verbose.
func runDiff(a, b string) (from, to string, ok bool) {
	return runDiffIn(a, b, func(run string) int { return strings.Count(a, run) })
}

// runDiffIn is runDiff with the uniqueness question asked of a wider scope.
//
// Inside a list, `substitute` walks EVERY element, so a run has to be unique
// across the list and not merely inside the element that changed. Widening
// only against its own element is why ten filenames each gaining `.patch`
// could not be expressed: the naive run is the trailing character, which
// occurs everywhere, and giving up on the first one sent the whole
// thirteen-line script down the whole-list assignment path.
func runDiffIn(a, b string, count func(string) int) (from, to string, ok bool) {
	if a == b {
		return "", "", false
	}
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	// Back off to a rune boundary: half a rune is not a substring anyone can
	// search for. The `i < len(a)` is not decoration: when one string is a
	// PREFIX of the other, i stops at len(a) and indexing there panics.
	for i > 0 && i < len(a) && !utf8Start(a[i]) {
		i--
	}
	j := 0
	for j < len(a)-i && j < len(b)-i && a[len(a)-1-j] == b[len(b)-1-j] {
		j++
	}
	for j > 0 && len(a)-j < len(a) && !utf8Start(a[len(a)-j]) {
		j--
	}
	from, to = a[i:len(a)-j], b[i:len(b)-j]
	// Widen until the run satisfies all three things a substitution owes.
	//
	//  1. it occurs exactly once in the scope it will be searched;
	//  2. replacing it REPRODUCES the target — uniqueness is not enough, and
	//     the difference bit: for "ααα" → "αααβ" the naive run is "αα", which
	//     strings.Count says occurs once (non-overlapping) while Replace puts
	//     it at the FRONT and yields "ααβα";
	//  3. it is GONE from the result, so applying the substitution a second
	//     time does nothing.
	//
	// The third is not decoration. Derived naively, the cargo `--locked` fix
	// came out as `"l --"` → `"l --locked --"`, and "l --" is still there
	// afterwards, inside "install --locked": running it twice gives
	// `--locked --locked`. Measured on the real overrides, 24 operations across
	// 22 projects did that. An override must be safe to apply to a recipe that
	// already has it, or nothing may ever apply one twice — not a re-run, not a
	// second tool, not the two formats side by side during a migration.
	//
	// Widening ALTERNATES, one rune to the right then one to the left.
	//
	// Going all the way right before touching the left overshoots badly: for
	// `cargo install --path .` it produces a run of most of the command, and
	// `worthSubstituting` then rejects it and assigns the whole value. A run
	// that straddles the insertion point is both idempotent and short —
	// `install --root` is not inside `cargo install --locked --root=x` — and
	// short is what a person reads.
	right := true
	for count(from) != 1 || strings.Replace(a, from, to, 1) != b || strings.Contains(b, from) {
		switch {
		case right && j > 0, j > 0 && i == 0:
			right = false
			j--
			for j > 0 && !utf8Start(a[len(a)-j]) {
				j--
			}
		case i > 0:
			right = true
			i--
			for i > 0 && !utf8Start(a[i]) {
				i--
			}
		default:
			// The run is the whole of `a` and it still fails one of the three.
			// A pure insertion is the ordinary case: "abc" becoming "abcdef"
			// has no run whose removal is idempotent, because the original is
			// a substring of the result. The caller assigns the value instead,
			// which is always correct and merely blunter.
			return "", "", false
		}
		from, to = a[i:len(a)-j], b[i:len(b)-j]
	}
	// No `from == ""` check: the loop above exits only when the run occurs
	// exactly once, and strings.Count reports len(a)+1 occurrences of the
	// empty string.
	return snapToWords(a, b, i, j)
}

// snapToWords grows a run out to whitespace, when that keeps every property.
//
// The shortest run that satisfies the three rules is often a fragment —
// `l --roo` rather than `install --root` — and these files are read by people.
// Widening can only help: a longer run occurs no more often than a shorter one
// and is no more present in the result, so the only thing to re-check is that
// the replacement still reproduces the target.
func snapToWords(a, b string, i, j int) (string, string, bool) {
	// Growing needs no re-checking, and the version that re-checked had four
	// branches no input could reach. Each property survives a longer run:
	//
	//   - a superstring occurs no more often than the string, and it occurred
	//     exactly once, and it still occurs at least once — so still once;
	//   - a superstring of something absent from the result is absent too;
	//   - the replacement still reproduces, because growing only moves the
	//     edges INTO the common prefix and the common suffix, where a and b
	//     agree character for character.
	//
	// The loops stop at i == 0 and j == 0, so neither can run off an end.
	for i > 0 && a[i-1] != ' ' && a[i-1] != '\n' {
		i--
	}
	for j > 0 && a[len(a)-j] != ' ' && a[len(a)-j] != '\n' {
		j--
	}
	return a[i : len(a)-j], b[i : len(b)-j], true
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }

// countInList counts a run across every string the list holds, including the
// string fields of a step written as a mapping — which is where substitute
// will look too.
func countInList(l []any, from string) int {
	n := 0
	for _, e := range l {
		switch v := e.(type) {
		case string:
			n += strings.Count(v, from)
		case map[string]any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					n += strings.Count(s, from)
				}
			}
		}
	}
	return n
}

// worthSubstituting asks the only question that distinguishes the two verbs:
// does any of the value SURVIVE?
//
// A substitution amends; an assignment decides. When the run runDiff settles
// on is the whole value there is nothing being amended, and `set = "^3"` says
// what happened far better than swapping "^1.1" for "^3" inside a string four
// characters long.
//
// This needs no length threshold, and the earlier one — "the part that stays
// must be at least as long as the part that changes" — stopped meaning
// anything once runs had to be idempotent and snapped to whitespace, because
// those are necessarily longer. It was rejecting `install --path` →
// `install --locked --path`, which is exactly the case substitution exists for.
//
// A value with no whitespace in it settles on itself, so a version constraint
// becomes a `set` without that being a special case.
func worthSubstituting(whole, from string) bool {
	return len(from) < len(whole)
}

// changedText pulls the one piece of text that differs between two list
// elements, whether the element is a plain command or a step written as a
// mapping.
//
// A pantry step is often `{run: "…", if: ">=3.4.3", working-directory: "…"}`,
// and treating that as opaque is what sent info-zip.org/zip — ten filenames
// gaining `.patch`, inside a list whose FIRST element is such a step — down
// the whole-list assignment path.
//
// Exactly one field may differ. Two would need two substitutions at one index,
// and the caller has no way to say which of them applies first.
func changedText(x, y any) (string, string, bool) {
	if xs, ok := x.(string); ok {
		ys, ok2 := y.(string)
		return xs, ys, ok2
	}
	xm, ok1 := x.(map[string]any)
	ym, ok2 := y.(map[string]any)
	if !ok1 || !ok2 || len(xm) != len(ym) {
		return "", "", false
	}
	var from, to string
	found := 0
	for k, xv := range xm {
		yv, in := ym[k]
		if !in {
			return "", "", false
		}
		if reflect.DeepEqual(xv, yv) {
			continue
		}
		xs, k1 := xv.(string)
		ys, k2 := yv.(string)
		if !k1 || !k2 {
			return "", "", false
		}
		found++
		from, to = xs, ys
	}
	if found != 1 {
		return "", "", false
	}
	return from, to, true
}
