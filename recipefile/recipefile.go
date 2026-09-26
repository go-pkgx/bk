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

// LoadOverlay reads a project's recipe, consulting an OVERLAY checkout before
// the pantry — which is what the factory does, through PKGX_PANTRY_OVERLAY.
//
// A tool that reads only the pantry describes a build nobody performs. The
// s390x seed order was computed that way and missed
// github.com/besser82/libxcrypt entirely: perl.org declares it in our overlay,
// with a comment explaining that the published perl bottle NEEDs libcrypt.so.1
// and glibc dropped it. Upstream's perl.org says nothing about it, so a walk
// over upstream alone cannot see the edge — and the build that discovered it
// was three steps downstream, in gnu.org/gcc.
//
// An empty overlay is the pantry-only case, so callers need not branch.
func LoadOverlay(overlayDir, pantryDir, project string) (*pantry.Recipe, error) {
	if overlayDir != "" {
		if r, err := Load(overlayDir, project); err == nil {
			return r, nil
		} else if !errors.Is(err, ErrNoRecipe) {
			// The overlay HAS this recipe and it does not parse. Falling
			// through to the pantry would build something other than what the
			// overlay says, silently.
			return nil, err
		}
	}
	return Load(pantryDir, project)
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
func LoadFile(path string) (*pantry.Recipe, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(b, filepath.Base(path))
}

// parse picks the front-end. Both end at pantry.Parse and the same JSON
// Schema — hcl.Parse round-trips through YAML precisely so a recipe written
// either way cannot validate differently.
func parse(b []byte, name string) (*pantry.Recipe, error) {
	if filepath.Ext(name) == ".hcl" {
		return hcl.Parse(b, name)
	}
	return pantry.Parse(b)
}
