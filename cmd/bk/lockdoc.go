package main

import (
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/go-pkgx/bk/logical"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bk/versions"
	"github.com/go-pkgx/bottle"
)

// lockedPin is one project as the lock pins it.
type lockedPin struct{ Project, Version, Spec string }

// lockDoc is a whole lock: what was asked for, what it resolved to, and the
// facts a later reader needs to decide whether the answer still holds.
//
// # THE HEADER IS DATA, NOT A COMMENT
//
// The first version of this wrote the platform, the roots and the two
// revisions as `#` lines. They were the right facts in the wrong place: a
// comment cannot be read back, so `--check` would have had to re-derive them
// from arguments the caller might give differently, and the check would then
// be of a different question from the one the file answers.
//
// Spack's lockfile is the precedent for every field here. Its `_meta` carries
// a `lockfile-version`; it records "spack version/commit in spack.lock to
// track information that should enhance reproducibility"; and its top level
// holds `roots` beside `concrete_specs`. The compatibility rule that comes
// with it is worth copying too — new readers read old locks, old readers
// refuse new ones — and `lockfile_version` is what makes that possible later
// without a guess.
type lockDoc struct {
	Version   int    // lockfile_version
	Platform  string // the platform it was taken on: it CHANGES the answer
	Generated string // RFC3339, UTC
	BK        string // which bk wrote it
	Roots     []string
	Pantry    string
	Overlay   string
	Pins      []lockedPin
}

// lockfileVersion is this format's number. Bumped when a reader of the
// previous one would MISREAD a file rather than merely miss a field.
const lockfileVersion = 1

// bkVersion is the module version of the running bk, which is what Spack
// records for the same reason: a lock is evidence about a resolution, and the
// resolver is part of it. A local build has no version stamped in, and saying
// `(devel)` is more honest than leaving the field out.
func bkVersion() string {
	if bi, ok := buildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(unknown)"
}

// buildInfo is a seam: a test binary's own build info is not bk's, and both
// branches above have to be reachable.
var buildInfo = debug.ReadBuildInfo

// resolveLock turns a closure order into pins, hashing as it goes.
//
// `order` is topological, deps first, so a dependency's spec hash is always
// already in hand when its dependent is hashed. That is why this loop is not
// sorted and the OUTPUT is.
func resolveLock(set *logical.Set, overlayDir, pantryDir string, tgt target.Target,
	order []string, read map[string][]*pantry.Recipe) ([]lockedPin, []string) {
	var pins []lockedPin
	var unresolved []string
	hashes := map[string]string{}
	for _, proj := range order {
		r, _, err := recipeLoader(set, overlayDir, pantryDir, proj)
		if err != nil || r == nil {
			unresolved = append(unresolved, fmt.Sprintf("%s (no recipe: %v)", proj, err))
			continue
		}
		v, _, err := versions.Resolve(r.Versions, "")
		if err != nil || v == "" {
			unresolved = append(unresolved, fmt.Sprintf("%s (%v)", proj, err))
			continue
		}
		// The recipes the closure itself read. Not re-read: the walk already
		// has them, and a project whose recipes do not load never reaches
		// `order`, so a second read could only fail in ways nothing can test.
		recs := read[proj]
		sh, err := specHash(recs, proj, v, tgt, hashes, specDeps(recs, tgt))
		if err != nil {
			unresolved = append(unresolved, fmt.Sprintf("%s (%v)", proj, err))
			continue
		}
		hashes[proj] = sh
		pins = append(pins, lockedPin{proj, v, sh})
	}
	// Sorted, not closure order: a lock is a SET of facts and is read as a
	// diff. Topological order changes when an unrelated dependency moves, and
	// every line would then appear to have changed.
	sort.Slice(pins, func(i, j int) bool { return pins[i].Project < pins[j].Project })
	return pins, unresolved
}

// renderLock writes the lock. HCL, because every other file this factory
// reads by hand is HCL and `bottle.HCLToMap` can read it straight back.
func renderLock(d lockDoc) string {
	var b strings.Builder
	b.WriteString("# bk lock\n#\n" +
		"# `spec` is a Merkle hash over the platform, the name, the resolved\n" +
		"# version, the PARSED recipe (so reformatting does not move it) and the\n" +
		"# spec hashes of the dependencies — Spack's shape, whose packaging guide\n" +
		"# counts \"a canonical hash of the package.py recipes\" among a spec hash's\n" +
		"# inputs. A version alone calls two builds the same when a build script\n" +
		"# changed under them.\n#\n" +
		"# What this still does NOT pin: the BYTES of the built bottle. The spec\n" +
		"# hash covers the inputs, as Nix's and Spack's do; an output digest is a\n" +
		"# different promise and is not made here.\n#\n" +
		"# Sorted by project, not in build order: a lock is read as a diff, and a\n" +
		"# topological order makes every line move when one dependency does.\n#\n" +
		"# `bk lock --check <this file>` re-resolves and says what moved.\n\n")
	fmt.Fprintf(&b, "lockfile_version = %d\n", d.Version)
	fmt.Fprintf(&b, "bk               = %q\n", d.BK)
	fmt.Fprintf(&b, "platform         = %q\n", d.Platform)
	fmt.Fprintf(&b, "generated        = %q\n", d.Generated)
	fmt.Fprintf(&b, "pantry           = %q\n", d.Pantry)
	fmt.Fprintf(&b, "overlay          = %q\n", d.Overlay)
	b.WriteString("roots            = [")
	for i, r := range d.Roots {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", r)
	}
	b.WriteString("]\n\nlocked = {\n")
	for _, p := range d.Pins {
		fmt.Fprintf(&b, "  %q = { version = %q, spec = %q }\n", p.Project, p.Version, p.Spec)
	}
	b.WriteString("}\n")
	return b.String()
}

// readLock parses a lock back.
//
// Through bottle.HCLToMap, the same reader the pantry overlay's HCL recipes
// go through, so the lock cannot develop a dialect of its own — and so this
// costs no new dependency.
func readLock(path string) (lockDoc, error) {
	src, err := osReadFile(path)
	if err != nil {
		return lockDoc{}, err
	}
	m, err := bottle.HCLToMap(src, path)
	if err != nil {
		return lockDoc{}, err
	}
	d := lockDoc{
		Version:   int(asInt(m["lockfile_version"])),
		Platform:  asString(m["platform"]),
		Generated: asString(m["generated"]),
		BK:        asString(m["bk"]),
		Pantry:    asString(m["pantry"]),
		Overlay:   asString(m["overlay"]),
	}
	for _, r := range asSlice(m["roots"]) {
		d.Roots = append(d.Roots, asString(r))
	}
	// A lock with no readable `locked` block is not an empty lock. Treated as
	// an error for the same reason `bk lock` refuses to pin nothing: the
	// caller would read "nothing moved" off a file that says nothing.
	locked, ok := m["locked"].(map[string]any)
	if !ok || len(locked) == 0 {
		return lockDoc{}, fmt.Errorf("%s: no `locked` entries — this is not a lock", path)
	}
	for proj, v := range locked {
		e, ok := v.(map[string]any)
		if !ok {
			return lockDoc{}, fmt.Errorf("%s: %q is not a { version = …, spec = … } entry", path, proj)
		}
		d.Pins = append(d.Pins, lockedPin{proj, asString(e["version"]), asString(e["spec"])})
	}
	sort.Slice(d.Pins, func(i, j int) bool { return d.Pins[i].Project < d.Pins[j].Project })
	if d.Version > lockfileVersion {
		return lockDoc{}, fmt.Errorf("%s: lockfile_version %d, and this bk understands %d — "+
			"read it with a newer bk rather than with this one, which would miss whatever the "+
			"bump was for", path, d.Version, lockfileVersion)
	}
	return d, nil
}

// asString, asInt and asSlice read a value HCLToMap produced. They do not
// report a type error: a field of the wrong type reads as its zero, and the
// comparison then SAYS so by name instead of refusing the whole file for one
// line. The one shape that cannot be tolerated — a missing `locked` — is
// checked above.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// lockDrift is one disagreement between a lock and today.
type lockDrift struct{ project, was, now string }

// compareLock says what moved between a lock and a fresh resolution.
//
// # THE INTERESTING CASE IS THE ONE A VERSION CANNOT SEE
//
// A project whose VERSION is unchanged and whose SPEC HASH moved is a recipe
// that was edited under a pin. That is the case the spec hash exists for, and
// it is reported in its own words rather than as a hash diff nobody reads.
func compareLock(was, now []lockedPin) []lockDrift {
	index := func(ps []lockedPin) map[string]lockedPin {
		m := make(map[string]lockedPin, len(ps))
		for _, p := range ps {
			m[p.Project] = p
		}
		return m
	}
	a, b := index(was), index(now)
	var out []lockDrift
	for _, p := range was {
		q, ok := b[p.Project]
		if !ok {
			out = append(out, lockDrift{p.Project, p.Version, "(gone from the closure)"})
			continue
		}
		switch {
		case p.Version != q.Version:
			out = append(out, lockDrift{p.Project, p.Version, q.Version})
		case p.Spec != q.Spec:
			out = append(out, lockDrift{p.Project,
				"spec " + shortHash(p.Spec),
				"spec " + shortHash(q.Spec) + " — same version, so something it is built FROM changed"})
		}
	}
	for _, q := range now {
		if _, ok := a[q.Project]; !ok {
			out = append(out, lockDrift{q.Project, "(not in the lock)", q.Version})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].project < out[j].project })
	return out
}

// lockAge is how old a lock is, for a line that says so. A reader deciding
// whether to trust a lock wants the age, not the timestamp arithmetic.
func lockAge(generated string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, generated)
	if err != nil {
		return "unknown age"
	}
	d := now.UTC().Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s) old", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hour(s) old", int(d.Hours()))
	}
	return fmt.Sprintf("%d day(s) old", int(d.Hours()/24))
}

// checkLock re-resolves a lock's own roots and says what moved.
//
// # WHOSE IDEA THIS IS
//
// Cargo's `--locked` and npm's `ci` both exist because a lock nobody verifies
// is a lock nobody can rely on: they refuse to run when the lock and a fresh
// resolution disagree. Spack keeps the same shape from the other end — it
// reuses a lockfile's concrete specs unless a run forces re-concretization.
//
// What is reported, and in this order of interest:
//
//	a version that moved          — the ordinary case, and the one a
//	                                version-only lock could also catch
//	a SPEC HASH that moved while  — a recipe edited UNDER a pin. This is the
//	the version did not             case the spec hash exists for, and the
//	                                only one nothing else here can see
//	a project that left or joined — the closure's SHAPE changed
//	a revision that differs       — said, never fatal on its own: a pantry
//	                                can move without moving any answer, and
//	                                calling that drift would make the check
//	                                cry wolf until nobody ran it
func checkLock(want lockDoc, roots []string, pantryDir, overlayDir, overridesDir, platform string,
	stdout, stderr io.Writer) int {
	lset, err := logical.LoadDir(overridesDir)
	if err != nil {
		fmt.Fprintln(stderr, "lock:", err)
		return 2
	}
	osn, arch, _ := strings.Cut(platform, "/")
	tgt := target.Target{Platform: osn, Arch: arch}
	warn := func(s string) { fmt.Fprintln(stderr, s) }

	expanded := expandSets(lset, overlayDir, pantryDir, closureRoots(roots, warn), warn)
	order, _, read := closureOf(lset, overlayDir, pantryDir, tgt, expanded, warn)
	if len(order) == 0 {
		fmt.Fprintln(stderr, "lock: the closure is empty — nothing was read, so this says nothing about the lock")
		return 2
	}
	now, unresolved := resolveLock(lset, overlayDir, pantryDir, tgt, order, read)

	fmt.Fprintf(stdout, "lock: %s · %s · %d pinned, %s\n",
		platform, want.Generated, len(want.Pins), lockAge(want.Generated, lockNow()))
	for _, r := range []struct{ name, was, now string }{
		{"pantry", want.Pantry, gitRevOf(pantryDir)},
		{"overlay", want.Overlay, gitRevOf(overlayDir)},
	} {
		if r.was != r.now {
			fmt.Fprintf(stdout, "lock: %s revision differs: %s → %s\n", r.name, r.was, r.now)
		}
	}
	// An unresolved project is not drift — it is an answer nobody got — and
	// it must not be folded into the moved count, or a registry outage would
	// read as a pantry that changed.
	for _, u := range unresolved {
		fmt.Fprintf(stderr, "lock: unresolved: %s\n", u)
	}

	drift := compareLock(want.Pins, now)
	if len(drift) == 0 && len(unresolved) == 0 {
		fmt.Fprintf(stdout, "lock: %d project(s), none moved\n", len(now))
		return 0
	}
	for _, d := range drift {
		fmt.Fprintf(stdout, "  %-34s %s → %s\n", d.project, d.was, d.now)
	}
	if len(drift) > 0 {
		fmt.Fprintf(stdout, "lock: %d of %d moved\n", len(drift), len(want.Pins))
	}
	return 1
}
