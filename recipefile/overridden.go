package recipefile

import (
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
	dir := Dir(pantryDir, project)
	for _, n := range Names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if set.For(project) == nil {
			return parse(b, n)
		}
		return overridden(set, project, b, n)
	}
	return nil, fmt.Errorf("%w for %s in %s (tried %v)", ErrNoRecipe, project, dir, Names)
}

// overridden applies the override between reading the file and validating it,
// so the result goes through the same schema every other recipe does. An
// override that produces something a recipe may not say fails here, loudly,
// rather than at the point the build trips over it.
func overridden(set *logical.Set, project string, b []byte, name string) (*pantry.Recipe, error) {
	y := b
	if filepath.Ext(name) == ".hcl" {
		// Through bottle, the same converter the client uses: a second reading
		// of one format is two opinions about what a recipe means.
		var err error
		if y, err = bottle.HCLToYAML(b, name); err != nil {
			return nil, err
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(y, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if _, err := set.ApplyTo(project, doc); err != nil {
		return nil, err
	}
	out, err := yamlMarshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return pantry.Parse(out)
}
