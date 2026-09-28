package logical

import (
	"fmt"
	"reflect"
)

// Outcome is what one operation did, and the three cases are the reason this
// package exists.
//
// A unified diff has two: it applied, or it did not. "Did not" hides the
// difference between a defect and a job already done, and that difference is
// the whole maintenance story of an override directory.
type Outcome int

const (
	// Applied: the document changed.
	Applied Outcome = iota
	// Redundant: the document ALREADY said this. Upstream has caught up, and
	// the override should be deleted rather than carried. Never an error.
	Redundant
	// PremiseGone: what this operation edits is not there any more. It is an
	// error, and it is the only one of the three a diff could tell you about.
	PremiseGone
)

func (o Outcome) String() string {
	switch o {
	case Applied:
		return "applied"
	case Redundant:
		return "redundant"
	default:
		return "premise gone"
	}
}

// Op is one operation. Exactly one of the verbs is set.
type Op struct {
	// Why this operation exists. Required: an override whose reason lives only
	// in a commit message is an override nobody can decide to delete.
	Why string

	Path Path

	Set any // set Path to this value (maps deep-merge, lists replace)
	// Expect is what the operation believes is at Path before it runs, and
	// HasExpect says the field means anything — nil is a value a document can
	// legitimately hold.
	//
	// It exists for the one operation that can swallow an upstream change
	// without saying so: replacing a whole list that was already there. A
	// unified diff would at least have refused; a blind assignment would not,
	// and quietly discarding somebody else's fix is worse than failing.
	// RFC 6902 has this as `test`, and it is the verb JSON Merge Patch lacks.
	Expect     any
	HasExpect  bool
	Remove     bool   // remove Path
	Append     []any  // append these to the list at Path
	Prepend    []any  // prepend these to the list at Path
	From, To   string // substitute: replace From with To inside the string(s) at Path
	Substitute bool   // distinguishes a substitution with an empty To
}

// Result reports one operation's outcome.
type Result struct {
	Op      Op
	Outcome Outcome
	Detail  string
}

// Apply runs ops against doc, in order, and reports what each one did.
//
// It returns an error on the FIRST premise that is gone, with nothing further
// attempted: a half-applied override is a recipe nobody wrote, and building
// from one is worse than not building.
func Apply(doc map[string]any, ops []Op) ([]Result, error) {
	var out []Result
	for _, op := range ops {
		r, err := applyOne(doc, op)
		out = append(out, r)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func applyOne(doc map[string]any, op Op) (Result, error) {
	switch {
	case op.Remove:
		return remove(doc, op)
	case op.Substitute:
		return substitute(doc, op)
	case op.Append != nil || op.Prepend != nil:
		return insert(doc, op)
	default:
		return set(doc, op)
	}
}

// parentOf walks to the map holding the last step, creating maps on the way
// only when the caller is allowed to (set), never otherwise.
func parentOf(doc map[string]any, p Path, create bool) (map[string]any, string, error) {
	cur := doc
	for i := 0; i < len(p)-1; i++ {
		step := p[i]
		next, ok := cur[step]
		if !ok {
			if !create {
				return nil, "", fmt.Errorf("%s: %s is not there", p, Path(p[:i+1]))
			}
			m := map[string]any{}
			cur[step] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("%s: %s is a %T, not a mapping", p, Path(p[:i+1]), next)
		}
		cur = m
	}
	return cur, p[len(p)-1], nil
}

func set(doc map[string]any, op Op) (Result, error) {
	parent, key, err := parentOf(doc, op.Path, true)
	if err != nil {
		return Result{op, PremiseGone, err.Error()}, err
	}
	old, had := parent[key]
	if op.HasExpect && had && !reflect.DeepEqual(old, op.Expect) && !reflect.DeepEqual(old, op.Set) {
		// Not what we thought we were replacing, and not the result either.
		// Somebody changed it, and overwriting would throw their change away.
		return Result{op, PremiseGone, fmt.Sprintf("%s is not what this replaces any more", op.Path)},
			fmt.Errorf("%s is not what this replaces any more", op.Path)
	}
	want := op.Set
	if m, ok := want.(map[string]any); ok {
		if oldM, ok2 := old.(map[string]any); ok2 {
			want = mergeMaps(oldM, m)
		}
	}
	if had && reflect.DeepEqual(old, want) {
		return Result{op, Redundant, "the recipe already says this"}, nil
	}
	parent[key] = want
	return Result{op, Applied, ""}, nil
}

// mergeMaps deep-merges b over a, returning a new map. A list in b replaces the
// list in a — see the package comment for why lists are not merged.
func mergeMaps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if bm, ok := v.(map[string]any); ok {
			if am, ok2 := out[k].(map[string]any); ok2 {
				out[k] = mergeMaps(am, bm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func remove(doc map[string]any, op Op) (Result, error) {
	parent, key, err := parentOf(doc, op.Path, false)
	if err != nil {
		// A parent that is gone means the key is gone: the removal has nothing
		// to do, which is redundant rather than broken. Removing something
		// twice must be safe, or an override cannot be idempotent.
		return Result{op, Redundant, "nothing at " + op.Path.String()}, nil
	}
	if _, had := parent[key]; !had {
		return Result{op, Redundant, "nothing at " + op.Path.String()}, nil
	}
	delete(parent, key)
	return Result{op, Applied, ""}, nil
}

func insert(doc map[string]any, op Op) (Result, error) {
	parent, key, err := parentOf(doc, op.Path, false)
	if err != nil {
		return Result{op, PremiseGone, err.Error()}, err
	}
	cur, had := parent[key]
	if !had {
		return Result{op, PremiseGone, op.Path.String() + " is not there to add to"},
			fmt.Errorf("%s is not there to add to", op.Path)
	}
	list, ok := cur.([]any)
	if !ok {
		return Result{op, PremiseGone, fmt.Sprintf("%s is a %T, not a list", op.Path, cur)},
			fmt.Errorf("%s is a %T, not a list", op.Path, cur)
	}
	add := op.Append
	if op.Prepend != nil {
		add = op.Prepend
	}
	// Already there, by VALUE: adding the same element twice is what turns an
	// override that ran into an override that runs every time.
	present := 0
	for _, want := range add {
		for _, have := range list {
			if reflect.DeepEqual(have, want) {
				present++
				break
			}
		}
	}
	if present == len(add) {
		return Result{op, Redundant, "the list already holds these"}, nil
	}
	if op.Prepend != nil {
		parent[key] = append(append([]any{}, add...), list...)
	} else {
		parent[key] = append(append([]any{}, list...), add...)
	}
	return Result{op, Applied, ""}, nil
}
