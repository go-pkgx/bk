// Package recipefile is the single place that decides what a recipe file is
// called and how it is read.
//
// Before this, four call sites wrote "package.yml" independently — the
// factory, the closure walk, depgaps' tree walk and the build's own reader —
// and pantry/hcl, an HCL2 front-end written in August that decodes into the
// same validated pantry.Recipe, was imported by nothing at all. It could not
// be reached without making four copies of a filename agree, so it never was.
//
// A filename written in four places is the same defect as a rule restated at
// four call sites: it is correct until one of them moves. depgaps is the
// illustration — it walks the tree looking for entries NAMED package.yml, so
// an HCL recipe would have been invisible to it while building perfectly
// everywhere else.
package recipefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/pantry/hcl"
)

// ErrNoRecipe is "this project has no recipe file here", which is not the same
// as "its recipe does not parse".
//
// The factory treats them differently and must keep doing so: an absent recipe
// is SKIPped without comment — a pantry need not be complete, and a closure
// walk names projects that resolve from upstream — while a malformed one is a
// recorded failure. Collapsing both into "could not load" would make a broken
// recipe disappear from the failures list instead of appearing in it.
var ErrNoRecipe = errors.New("no recipe file")

// Names are the recipe filenames, in the order they are tried. HCL first: a
// project that has both is mid-conversion, and the converted file is the one
// its author is working on.
var Names = []string{"package.hcl", "package.yml"}

// IsRecipe reports whether a directory entry is a recipe file, for a caller
// walking a pantry tree rather than asking about a known project.
func IsRecipe(name string) bool {
	for _, n := range Names {
		if name == n {
			return true
		}
	}
	return false
}

// Dir is the directory a project's recipe lives in.
func Dir(pantryDir, project string) string {
	return filepath.Join(pantryDir, "projects", filepath.FromSlash(project))
}

// Load reads a project's recipe from a pantry checkout.
//
// The error names the project and every filename tried, because "no such file
// or directory: …/package.yml" on a pantry that might hold either one reads as
// a missing project when it may be a misnamed file.
func Load(pantryDir, project string) (*pantry.Recipe, error) {
	dir := Dir(pantryDir, project)
	for _, n := range Names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		return parse(b, n)
	}
	return nil, fmt.Errorf("%w for %s in %s (tried %v)", ErrNoRecipe, project, dir, Names)
}

// LoadFile reads one recipe by path, choosing the front-end from its
// extension. `bk build --recipe` takes a path rather than a project.
//
// The path may be the recipe's DIRECTORY, in which case Names decides which
// file to read — the same rule Load uses for a project in a pantry.
//
// ⛔⛔ BECAUSE A CALLER THAT SPELLS THE FILENAME HAS ALREADY CHOSEN THE FORMAT,
// AND WILL BE WRONG LATER. This package exists so one place decides what a
// recipe is called, and the spelling keeps escaping it anyway: four commands
// read `package.yml` directly until go-pkgx/bk#389, and
// `packages/.github/workflows/wasip1-c.yml` then spelled
// `/recipes/<slug>/package.yml` in the workflow — so converting that lane to
// HCL2 meant editing the workflow in the same commit, or shipping twelve
// recipes the lane could not build.
//
// A directory is the form a caller can pass without knowing the answer. The
// filename form stays: it is right when the file is genuinely what you have.
func LoadFile(path string) (*pantry.Recipe, error) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		for _, n := range Names {
			b, err := os.ReadFile(filepath.Join(path, n))
			if err != nil {
				continue
			}
			return parse(b, n)
		}
		return nil, fmt.Errorf("%w in %s (tried %v)", ErrNoRecipe, path, Names)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(b, filepath.Base(path))
}

// ParseNamed parses bytes a caller already holds, choosing the front-end from
// the FILENAME those bytes came from.
//
// It exists for a walker: `bk depgaps` finds recipes with IsRecipe and reads
// them itself, so it has the content and the name but no path to re-open. It
// used to call pantry.Parse — the YAML front-end — on every one of them, which
// made each `package.hcl` a parse error it then SKIPPED as malformed. Over
// pantry-overlay that was 210 of 213 recipes examined by nobody, reported as
// a clean result (go-pkgx/bk#389).
func ParseNamed(b []byte, name string) (*pantry.Recipe, error) { return parse(b, name) }

// parse picks the front-end. Both end at pantry.Parse and the same JSON
// Schema — hcl.Parse round-trips through YAML precisely so a recipe written
// either way cannot validate differently.
func parse(b []byte, name string) (*pantry.Recipe, error) {
	if filepath.Ext(name) == ".hcl" {
		return hcl.Parse(b, name)
	}
	return pantry.Parse(b)
}
