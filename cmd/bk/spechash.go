package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
)

// specHash is a Merkle hash over a project AS RESOLVED: its platform, its
// name, the version it resolved to, the content of the recipe (or recipes)
// that describe it, and the hashes of its dependencies.
//
// # WHOSE IDEA THIS IS
//
// Spack's, exactly. Its packaging guide states what goes into a spec hash:
// "build, link, and run dependencies all affect the hash of Spack packages
// (along with sha256 sums of patches and archives used to build the package,
// and a canonical hash of the package.py recipes)".
//
// The part worth copying is the last clause. Pinning versions catches an
// upstream that moved; hashing the RECIPE catches the other half — a build
// script edited in the pantry resolves to the same version and produces a
// different package. A lock with versions alone calls those two builds the
// same, which is the thing a lock exists not to do.
//
// # WHAT "CANONICAL" MEANS HERE, AND WHAT IT DOES NOT
//
// Spack canonicalises package.py's AST, dropping comments and docstrings.
// Here the recipe is parsed YAML or HCL, and the hash is taken over the
// PARSED value re-serialised as JSON with sorted keys. So reformatting a
// recipe, reordering its mapping keys or rewriting its comments does not
// move the hash, and changing what it says does.
//
// It is NOT a hash of the file. A recipe whose meaning is identical through
// two different spellings hashes alike, and that is deliberate: a lock that
// invalidated on whitespace would be re-taken until it stopped meaning
// anything.
//
// # BOTH HALVES, BECAUSE THE CLOSURE READS BOTH
//
// A project can be described by the pantry AND by our overlay, and
// closureRecipes returns both because they say different things. The hash
// covers both, in the order the closure reads them: a hash taken over one
// half would describe a different graph from the one beside it in the same
// file.
func specHash(recs []*pantry.Recipe, proj, version string, tgt target.Target, depHashes map[string]string, deps []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "bk-lock-spec-hash v1\n")
	fmt.Fprintf(h, "platform %s/%s\n", tgt.Platform, tgt.Arch)
	fmt.Fprintf(h, "project %s\n", proj)
	fmt.Fprintf(h, "version %s\n", version)
	for i, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			// Not swallowed: a recipe that cannot be serialised has no hash,
			// and a project silently hashed over fewer of its halves than the
			// closure read is the defect this function exists to avoid.
			return "", fmt.Errorf("recipe half %d of %s: %w", i, proj, err)
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(h, "recipe %d %s\n", i, hex.EncodeToString(sum[:]))
	}
	for _, d := range deps {
		dh, ok := depHashes[d]
		if !ok {
			// A dependency the closure could not read — it resolves from the
			// upstream dist at build time — is recorded as unknown rather than
			// omitted. Omitting it would make "we have no recipe for X" and "X
			// is not a dependency" hash alike; and the day a recipe for it
			// arrives, the hash must move.
			dh = "(no recipe)"
		}
		fmt.Fprintf(h, "dep %s %s\n", d, dh)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// specDeps is the sorted, de-duplicated set of dependency NAMES across every
// half of a recipe, for tgt. It is the same reduction closureOf walks, so the
// hash describes the graph the lock lists and not a neighbouring one.
func specDeps(recs []*pantry.Recipe, tgt target.Target) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		for _, spec := range build.DepSpecs(r.Dependencies, tgt) {
			n := depName(spec)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// shortHash is the first 12 hex digits, the length git settled on for a
// human-readable prefix. The full hash is what is compared; this is what is
// printed when a line has to fit on a screen.
func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
