package recipefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-pkgx/bk/logical"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bottle"
	"gopkg.in/yaml.v3"
)

// yamlMarshal is a seam. Marshalling a document a parser produced cannot
// realistically fail — every value in it came out of a decoder — but the error
// exists and swallowing it would mean feeding the schema an empty recipe.
var yamlMarshal = yaml.Marshal

// LoadOverridden reads a recipe and applies the project's LOGICAL override, if
// the set holds one.
//
// Nothing on disk is touched. The unified diffs this is replacing were applied
// by rewriting package.yml in the pantry checkout, which worked because a
// textual diff leaves alone everything it does not mention; a document rewrite
// would drop every comment in the file, and those comments carry the reasons —
// gnu.org/grep spends thirteen lines explaining why it names itself as a build
// dependency.
//
// A project the set does not mention takes exactly the path it took before:
// same bytes, same front-end, no document round trip. During the migration
// most projects are still unified diffs and must not change behaviour because
// this function exists.
func LoadOverridden(set *logical.Set, pantryDir, project string) (*pantry.Recipe, error) {
	r, _, err := LoadOverriddenReporting(set, pantryDir, project)
	return r, err
}

// LoadOverriddenReporting is LoadOverridden, saying what each operation did.
//
// `bk overrides` needs the outcomes — applied, already true, premise gone —
// and getting them by applying the override a SECOND time next to this call
// left two error branches no input could reach, because anything that fails
// there has already failed here. One read, one application, one answer.
func LoadOverriddenReporting(set *logical.Set, pantryDir, project string) (*pantry.Recipe, []logical.Result, error) {
	dir := Dir(pantryDir, project)
	for _, n := range Names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if set.For(project) == nil {
			r, err := parse(b, n)
			return r, nil, err
		}
		return overridden(set, project, b, n)
	}
	return nil, nil, fmt.Errorf("%w for %s in %s (tried %v)", ErrNoRecipe, project, dir, Names)
}

// overridden applies the override between reading the file and validating it,
// so the result goes through the same schema every other recipe does. An
// override that produces something a recipe may not say fails here, loudly,
// rather than at the point the build trips over it.
func overridden(set *logical.Set, project string, b []byte, name string) (*pantry.Recipe, []logical.Result, error) {
	y := b
	if filepath.Ext(name) == ".hcl" {
		// Through bottle, the same converter the client uses: a second reading
		// of one format is two opinions about what a recipe means.
		var err error
		if y, err = bottle.HCLToYAML(b, name); err != nil {
			return nil, nil, err
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(y, &doc); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	res, err := set.ApplyTo(project, doc)
	if err != nil {
		return nil, res, err
	}
	out, err := yamlMarshal(doc)
	if err != nil {
		return nil, res, fmt.Errorf("%s: %w", name, err)
	}
	r, err := pantry.Parse(out)
	return r, res, err
}

// LoadBuildRecipe reads the recipe the FACTORY compiles.
//
// The pantry first, with the project's logical override applied; the overlay
// only when the pantry has no such project at all.
//
// FALLBACK, not preference — and the difference is the whole division of
// labour. For a project upstream carries, `overrides/` fixes the BUILD and the
// overlay fixes what a consumer RESOLVES, and letting the overlay win here
// would silently build something other than what the overrides say. For a
// project upstream does not carry, there is nothing to fall back from: the
// overlay's copy is the only recipe there is.
//
// It replaces a directory of patches that CREATED those recipes. A patch that
// adds a whole package.yml is not an override of anybody's recipe — it is a
// recipe of ours — and writing it twice, once as a diff for the builder and
// once as HCL for the consumer, is the two-halves defect carried deliberately.
// Measured on 2026-09-28: 8 projects were written both ways, and exactly 8 of
// the overlay's 183 are absent upstream, so this reaches those and nothing
// else.
func LoadBuildRecipe(set *logical.Set, overlayDir, pantryDir, project string) (*pantry.Recipe, error) {
	r, err := LoadOverridden(set, pantryDir, project)
	if !errors.Is(err, ErrNoRecipe) || overlayDir == "" {
		return r, err
	}
	return Load(overlayDir, project)
}
