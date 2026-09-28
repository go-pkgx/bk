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
		if op, ok := deriveList(al, bl, path, why); ok {
			*out = append(*out, op)
			return
		}
		*out = append(*out, Op{Why: why, Path: path, Set: b})
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
func deriveList(a, b []any, path Path, why string) (Op, bool) {
	// Pure append: a is a prefix of b.
	if len(b) > len(a) && reflect.DeepEqual(a, b[:len(a)]) {
		return Op{Why: why, Path: path, Append: append([]any{}, b[len(a):]...)}, true
	}
	// Pure prepend: a is a suffix of b.
	if len(b) > len(a) && reflect.DeepEqual(a, b[len(b)-len(a):]) {
		return Op{Why: why, Path: path, Prepend: append([]any{}, b[:len(b)-len(a)]...)}, true
	}
	// One element edited in place, and the edit is one contiguous run inside a
	// string: express it as the substitution it is.
	if len(a) == len(b) {
		changed := -1
		for i := range a {
			if !reflect.DeepEqual(a[i], b[i]) {
				if changed >= 0 {
					return Op{}, false
				}
				changed = i
			}
		}
		if changed >= 0 {
			as, ok1 := a[changed].(string)
			bs, ok2 := b[changed].(string)
			if ok1 && ok2 {
				if from, to, ok := runDiff(as, bs); ok && worthSubstituting(as, from) && countInList(a, from) == 1 {
					// Unique in the WHOLE list, not merely in the element that
					// changed: substitute walks every element, so a run that
					// also occurs in a neighbour would be replaced there too.
					// grpc.io and apache.org/serf both did exactly that, and
					// produced -DCMAKE_INSTALL_PREFIX=""{{prefix}} — found by
					// demanding the derived operations reproduce the diff, not
					// by reading this function.
					return Op{Why: why, Path: path, Substitute: true, From: from, To: to}, true
				}
			}
		}
	}
	return Op{}, false
}

// runDiff finds the single contiguous run by which two strings differ, and
// returns it as a from/to pair that is UNIQUE in the original — a substitution
// that matched twice would edit somewhere nobody looked.
//
// It refuses a run so short it would match by accident; the caller then falls
// back to assigning the whole value, which is always correct and merely more
// verbose.
func runDiff(a, b string) (from, to string, ok bool) {
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
	// Widen until replacing the run REPRODUCES the target. Uniqueness alone is
	// not enough, and the difference bit: for "ααα" → "αααβ" the naive run is
	// "αα", which strings.Count says occurs once — non-overlapping — while
	// Replace puts it at the FRONT and yields "ααβα". The only test worth
	// making is the one the caller will make.
	//
	// This terminates without a bound, and the guard that used to be here was
	// dead code. Each step moves i or j one rune towards 0, and when both
	// reach 0 the run is the whole of `a` and the replacement is the whole of
	// `b` — which reproduces by construction. A bound would have looked
	// prudent and been untestable.
	for strings.Count(a, from) != 1 || strings.Replace(a, from, to, 1) != b {
		if i > 0 {
			i--
			for i > 0 && !utf8Start(a[i]) {
				i--
			}
		} else {
			j--
			for j > 0 && !utf8Start(a[len(a)-j]) {
				j--
			}
		}
		from, to = a[i:len(a)-j], b[i:len(b)-j]
	}
	if from == "" {
		return "", "", false
	}
	return from, to, true
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
// would writing the new value out in full be REPEATING it?
//
// A substitution earns its keep on a 200-character build command that gains a
// flag — restating the command would fork it, and the fork is what rots. It
// earns nothing on a version constraint: `^1.1` becoming `^3` reads far better
// as `set = "^3"` than as a substring swap, and a short run is also the one
// most likely to match somewhere nobody looked.
//
// So: substitute only when the part that stays is at least as long as the part
// that changes. No threshold to argue about — the rule is the question.
func worthSubstituting(whole, from string) bool {
	return len(whole)-len(from) >= len(from)
}
