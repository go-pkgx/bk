package logical

import (
	"fmt"
	"strings"
)

// substitute replaces From with To inside the string at Path — or, when Path
// names a list, inside whichever of its elements contain From.
//
// Matching is on a SUBSTRING of a value, never on a position. The 42 overrides
// that need more than a merge are almost all this one shape: a flag appended
// to a command, a compiler swapped, a `cargo install` gaining `--locked`. A
// list index would express them too, and Kustomize's own documentation says
// why not to — reorder the base list and an index deletes the wrong element.
//
// The three outcomes are all real here:
//
//   - From is present     → replace it. Applied.
//   - To is already there and From is not → upstream did it. Redundant.
//   - neither             → the command this edits has changed underneath.
//     PremiseGone, which is an error, because silently
//     not applying is how the -Werror switch went missing.
func substitute(doc map[string]any, op Op) (Result, error) {
	if op.From == "" {
		return Result{op, PremiseGone, "substitute with an empty `from` would match everywhere"},
			fmt.Errorf("%s: substitute needs a `from`", op.Path)
	}
	parent, key, err := parentOf(doc, op.Path, false)
	if err != nil {
		return Result{op, PremiseGone, err.Error()}, err
	}
	cur, had := parent[key]
	if !had {
		return Result{op, PremiseGone, op.Path.String() + " is not there"},
			fmt.Errorf("%s is not there", op.Path)
	}

	switch v := cur.(type) {
	case string:
		out, n, already := subst1(v, op.From, op.To)
		if n == 0 {
			if already {
				return Result{op, Redundant, "the recipe already reads the new way"}, nil
			}
			return Result{op, PremiseGone, fmt.Sprintf("%s does not contain %q", op.Path, op.From)},
				fmt.Errorf("%s does not contain %q", op.Path, op.From)
		}
		parent[key] = out
		return Result{op, Applied, fmt.Sprintf("%d occurrence(s)", n)}, nil

	case []any:
		total, already := 0, false
		out := make([]any, len(v))
		copy(out, v)
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				// A list element can be a mapping — a `run:`/`if:` step. Reach
				// into its own string fields rather than skipping it, or every
				// conditional build step becomes unreachable.
				if m, ok2 := e.(map[string]any); ok2 {
					mm, n, a := substMap(m, op.From, op.To)
					total += n
					already = already || a
					out[i] = mm
				}
				continue
			}
			r, n, a := subst1(s, op.From, op.To)
			total += n
			already = already || a
			out[i] = r
		}
		if total == 0 {
			if already {
				return Result{op, Redundant, "the recipe already reads the new way"}, nil
			}
			return Result{op, PremiseGone, fmt.Sprintf("no element of %s contains %q", op.Path, op.From)},
				fmt.Errorf("no element of %s contains %q", op.Path, op.From)
		}
		parent[key] = out
		return Result{op, Applied, fmt.Sprintf("%d occurrence(s)", total)}, nil

	default:
		return Result{op, PremiseGone, fmt.Sprintf("%s is a %T, not text", op.Path, cur)},
			fmt.Errorf("%s is a %T, not text", op.Path, cur)
	}
}

// subst1 reports the replacement, how many occurrences it made, and whether
// the string ALREADY reads the new way — which is a different fact from having
// nothing to do, and the caller reports it differently.
func subst1(s, from, to string) (string, int, bool) {
	n := strings.Count(s, from)
	if n == 0 {
		return s, 0, to != "" && strings.Contains(s, to)
	}
	return strings.ReplaceAll(s, from, to), n, false
}

// substMap walks a step written as a mapping (`run:`, `prop:`, `working-directory:`).
func substMap(m map[string]any, from, to string) (map[string]any, int, bool) {
	out := make(map[string]any, len(m))
	total, already := 0, false
	for k, v := range m {
		r, n, a := substAny(v, from, to)
		total += n
		already = already || a
		out[k] = r
	}
	return out, total, already
}

// substAny is substMap's recursion, and the reason it exists is a step
// written one of the two equivalent ways:
//
//   - run: |              <- a STRING: reachable
//     for s in $SCRIPTS; do
//     sed -i …
//     done
//
//   - run:                <- a LIST: was copied through untouched
//
//   - for s in $SCRIPTS; do
//
//   - sed -i …
//
//   - done
//
// substMap only looked at string FIELDS, so the second form was invisible to
// every override and the attempt reported PremiseGone — an error, at least,
// rather than a silent miss. Measured on pantry 2df061b: 404 `run:` steps
// across 223 recipes are written as a list and 1539 as a string, so better
// than a fifth of the script surface could not be edited for a reason that
// is about YAML style and nothing else.
//
// The reach is now the same at any depth, which is what substMap's own
// comment already claimed: "reach into its own string fields rather than
// skipping it, or every conditional build step becomes unreachable."
func substAny(v any, from, to string) (any, int, bool) {
	switch x := v.(type) {
	case string:
		r, n, a := subst1(x, from, to)
		return r, n, a
	case []any:
		out := make([]any, len(x))
		total, already := 0, false
		for i, e := range x {
			r, n, a := substAny(e, from, to)
			total += n
			already = already || a
			out[i] = r
		}
		return out, total, already
	case map[string]any:
		// A mapping nested inside the targeted node. No recipe shape in this
		// pantry reaches here today — a `run:` list holds strings — but
		// stopping at one level is the defect being fixed, one level down.
		out, n, a := substMap(x, from, to)
		return out, n, a
	default:
		return v, 0, false
	}
}
