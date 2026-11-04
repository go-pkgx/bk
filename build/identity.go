package build

import (
	"sort"

	"github.com/go-zpkg/zbk/moustache"
	"github.com/go-zpkg/zbk/pantry"
	"github.com/go-zpkg/zbk/spec"
	"github.com/go-zpkg/zbk/target"
)

// identity is what a build IS: a Merkle hash over the platform, the project,
// the resolved version, the canonical form of the recipe, and the resolved
// prefix of every dependency.
//
// ⛔⛔ A FUNCTION OF THE INPUTS, NEVER OF THE OUTPUT. Its short form goes in
// the install prefix, which is baked into every binary the build produces —
// so it has to be known before a compiler runs, and hashing the result into
// the result is circular. Nix hashes the derivation and Spack the concrete
// DAG for exactly this reason.
//
// ⛔ DEPENDENCIES ARE IDENTIFIED BY THEIR RESOLVED PREFIX, WHICH MAKES THIS
// MERKLE FOR FREE. Once a dependency's own prefix carries its own hash,
// hashing that prefix carries the dependency's whole identity with it,
// transitively — the same mechanism as a Nix derivation referencing its
// inputs by store path. While a dependency is still installed at an unhashed
// `v<version>`, the edge degrades to version-level precision and no further,
// and it repairs itself as the registry is rebuilt.
//
// ⛔ UNEXPORTED ON PURPOSE. It was exported for a while so the test harnesses
// that stub the install step could recompute the directory the build would
// use — which was a second definition of the identity, free to drift from
// this one. They read `Runner.BuildInstall` instead, and callers outside the
// package get the identity through `Result.SpecHash`, which is the value this
// build actually used rather than one recomputed from arguments that may no
// longer match.
func identity(recipe *pantry.Recipe, project, version string, tgt target.Target, deps []moustache.Dep) (string, error) {
	ids := make([]string, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.Project+" "+d.Path)
	}
	// Sorted, because a hash that depended on the order a map was walked
	// would differ between two runs of the same build — which is the one
	// thing an identity must never do.
	sort.Strings(ids)
	return spec.Hash(spec.DomainBuild, []*pantry.Recipe{recipe}, project, version, tgt, ids)
}
