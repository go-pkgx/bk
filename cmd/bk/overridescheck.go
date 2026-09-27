package main

import (
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/go-pkgx/bk/overrides"
)

// runOverrides implements `bk overrides`: apply every patch in a directory to a
// pantry checkout and fail if any of them no longer applies.
//
// A patch stops applying when upstream edits the lines its context hangs on, or
// when one of ours does. Nothing about the build says so afterwards: the recipe
// is merely the unpatched one, and it fails for whatever reason the patch
// existed to remove. Measured 2026-09-24 on go-pkgx/packages, 3 of 231
// overrides had stopped — among them the -Werror switch mozilla.org/nss needs,
// whose absence cost two rebuilds and a duplicate fix for a defect that was
// already fixed.
//
// The factory already reports this, at build time, in a log nobody reads until
// something breaks. This is the same fact asked EARLY, by the same applier:
// overrides.Apply, not `git apply`. bk patches with go-gitdiff, and a check
// that shelled out to git would be a second opinion — one that can pass while
// the builder's own applier refuses, which is the failure it is meant to catch.
func runOverrides(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("overrides", flag.ContinueOnError)
	fset.SetOutput(stderr)
	dir := fset.String("dir", "overrides", "directory of *.patch recipe overrides")
	pantryDir := fset.String("pantry", envOr("PANTRY", "pantry"), "pantry checkout to patch — a FRESH one, since a patch already applied does not apply twice")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	res, err := overridesApply(overrides.Options{
		Dir:  *dir,
		Root: *pantryDir,
		Warn: func(s string) { fmt.Fprintln(stderr, s) },
	})
	if err != nil {
		fmt.Fprintf(stderr, "overrides: %v\n", err)
		return 1
	}

	// The DENOMINATOR, beside the count. "0 skipped" out of nothing read is
	// the same sentence as "0 skipped" out of every patch, and only one of
	// them is a pass — a `--dir` pointing at an empty directory would
	// otherwise report a clean run forever.
	total := len(res.Applied) + len(res.Skipped)
	if total == 0 {
		fmt.Fprintf(stderr, "overrides: no patch in %s — is that the right directory?\n", *dir)
		return 1
	}
	fmt.Fprintf(stdout, "%d override(s): %d applied, %d no longer apply\n", total, len(res.Applied), len(res.Skipped))
	if len(res.Skipped) == 0 {
		return 0
	}

	// Named by PROJECT as well as by patch: the patch name says which file was
	// meant to change, and the project says what will be built wrong.
	projects := make([]string, 0, len(res.SkippedProjects))
	for p := range res.SkippedProjects {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		names := append([]string(nil), res.SkippedProjects[p]...)
		sort.Strings(names)
		fmt.Fprintf(stderr, "  %s would build from an UNPATCHED recipe: %v\n", p, names)
	}
	return 1
}

// overridesApply is a seam: the failure paths here are a filesystem's, and a
// test should not need one to reach them.
var overridesApply = overrides.Apply
