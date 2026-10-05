package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-pkgx/bk/logical"
	"github.com/go-pkgx/bk/recipefile"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bk/versions"
)

// runLock implements `bk lock`: resolve a set (or a list of projects) to the
// exact versions it means TODAY, and write them down.
//
// # WHY A MANIFEST IS NOT ENOUGH, which is not our observation
//
// Guix's manual states it plainly: "to reproduce a profile bit-for-bit,
// manifests alone might not be enough" — the same names resolve differently
// against a different package set, so a manifest needs the channel revisions
// beside it. Spack names the two halves: an environment from `spack.yaml` has
// "the same root specs... may concretize differently", one from `spack.lock`
// has "the same concrete specs".
//
// `members` (go-pkgx/bk#294) gave this factory the abstract half. This is the
// concrete one.
//
// # AND IT MATTERS MORE HERE THAN IN GUIX
//
// A Guix manifest resolves deterministically once the channel revision is
// pinned: the version is in the checkout. A pkgx recipe's `versions:` spec
// asks GitHub for tags AT RESOLVE TIME, so pinning the pantry revision is not
// enough — the same pantry commit resolves `gnu.org/bash` to whatever the
// newest tag is the day you ask. The resolved version has to be written down
// or it is not pinned at all.
//
// # WHAT THIS PINS, AND WHAT IT DOES NOT
//
// It pins the NAMES, the VERSIONS and a per-project SPEC HASH (see specHash),
// and records the pantry and overlay revisions that produced them. The spec
// hash covers the INPUTS — platform, version, parsed recipe, dependencies —
// which is what Nix's derivation hash and Spack's spec hash cover too.
//
// It does not pin the OUTPUT bytes. A bottle digest would, and `bottle` can
// verify a digest but has no exported way to be asked for one; and a lock
// taken before anything is built has no output to name. Saying so is better
// than a lock that quietly guarantees less than a reader assumes.
func runLock(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pantryDir := fs.String("pantry", envOr("PANTRY", "pantry"), "pantry checkout dir")
	overlayDir := fs.String("overlay", envOr("PANTRY_OVERLAY_DIR", ""), "an overlay checkout consulted as the factory consults it")
	overridesDir := fs.String("overrides", envOr("OVERRIDES", ""), "directory of *.hcl logical recipe overrides")
	platform := fs.String("platform", envOr("PLATFORM", "linux/x86-64"), "target os/arch")
	out := fs.String("o", "", "write here instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "lock: name at least one project or set — a lock of nothing is not an empty lock, it is a mistake")
		return 2
	}
	// Go's flag package stops at the first non-flag argument, so a flag put
	// AFTER a project name is silently a project name. Measured on myself
	// within a minute of writing this command: `bk lock … go-pkgx/base-
	// toolchain -o out.hcl` wrote nothing to out.hcl and printed the lock to
	// stdout instead, because `-o` and `out.hcl` became two more roots.
	//
	// Refused rather than reordered: moving a user's arguments around is a
	// different command from the one they typed, and the day it guesses
	// wrong there is nothing in the output to say so.
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(stderr, "lock: %q comes after a project name, and Go's flag parsing "+
				"stops there — it would be read as a project. Put flags before the projects.\n", a)
			return 2
		}
	}
	lset, err := logical.LoadDir(*overridesDir)
	if err != nil {
		fmt.Fprintln(stderr, "lock:", err)
		return 2
	}
	osn, arch, _ := strings.Cut(*platform, "/")
	tgt := target.Target{Platform: osn, Arch: arch}
	warn := func(s string) { fmt.Fprintln(stderr, s) }

	roots := expandSets(lset, *overlayDir, *pantryDir, closureRoots(fs.Args(), warn), warn)
	order, _, read := closureOf(lset, *overlayDir, *pantryDir, tgt, roots, warn)
	if len(order) == 0 {
		fmt.Fprintln(stderr, "lock: the closure is empty — nothing was read, which is a failure and not a clean result")
		return 1
	}

	type pin struct{ project, version, spec string }
	var pins []pin
	var unresolved []string
	// Deps precede dependents in `order`, so a dependency's hash is always
	// already here when the dependent is hashed. That is the whole reason the
	// walk is topological and the reason this loop is not sorted.
	hashes := map[string]string{}
	for _, proj := range order {
		r, _, err := recipeLoader(lset, *overlayDir, *pantryDir, proj)
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
		pins = append(pins, pin{proj, v, sh})
	}
	// Sorted, not closure order: a lock is a SET of facts and is read as a
	// diff. Topological order changes when an unrelated dependency moves, and
	// every line would then appear to have changed.
	sort.Slice(pins, func(i, j int) bool { return pins[i].project < pins[j].project })

	var b strings.Builder
	fmt.Fprintf(&b, "# bk lock · %s · %s\n#\n", *platform, lockNow().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "# roots:   %s\n", strings.Join(fs.Args(), " "))
	fmt.Fprintf(&b, "# pantry:  %s\n", gitRevOf(*pantryDir))
	if *overlayDir != "" {
		fmt.Fprintf(&b, "# overlay: %s\n", gitRevOf(*overlayDir))
	}
	b.WriteString("#\n" +
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
		"# topological order makes every line move when one dependency does.\n\n")
	b.WriteString("locked = {\n")
	for _, p := range pins {
		fmt.Fprintf(&b, "  %q = { version = %q, spec = %q }\n", p.project, p.version, p.spec)
	}
	b.WriteString("}\n")

	for _, u := range unresolved {
		fmt.Fprintf(stderr, "lock: unresolved: %s\n", u)
	}
	if *out != "" {
		if err := osWriteFile(*out, []byte(b.String()), 0o644); err != nil {
			fmt.Fprintln(stderr, "lock:", err)
			return 1
		}
		fmt.Fprintf(stderr, "lock: %d project(s) pinned → %s\n", len(pins), *out)
	} else {
		fmt.Fprint(stdout, b.String())
	}
	// An unresolved project is a hole in the lock, and a lock with a hole must
	// not read as a success: the next person would commit it.
	if len(unresolved) > 0 {
		return 1
	}
	return 0
}

// lockNow and osWriteFile are seams: a timestamp and a write are the two
// things a test cannot let run free.
var (
	lockNow     = time.Now
	osWriteFile = os.WriteFile
	// recipeLoader is a seam: a project the closure walked but whose recipe
	// the per-project read cannot get is a HOLE in the lock, and the branch
	// that names it is not reachable with a real pantry.
	recipeLoader = recipefile.LoadBuildRecipe
)

// gitRevOf is the checkout's HEAD commit, or a word saying why not.
//
// Not an error: a pantry that is a plain directory — an unpacked tarball, a
// test fixture — is a legitimate thing to lock against, and refusing would
// make the tool unusable in exactly the setting where a lock matters most.
// But the lock must not claim a revision it does not have.
func gitRevOf(dir string) string {
	if dir == "" {
		return "(none)"
	}
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "(not a git checkout — this lock cannot be replayed from a revision)"
	}
	h, err := repo.Head()
	if err != nil {
		return "(no HEAD)"
	}
	return h.Hash().String()
}
