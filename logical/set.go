package logical

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Set is every logical override a directory holds, keyed by project.
//
// One file per project, not one per change: a project's overrides are read
// together, applied together, and argued about together. The unified diffs
// they replace were one per FIX, which is why llvm.org had four of them and
// why answering "what do we do to llvm.org?" meant opening four files and
// working out what order they applied in.
type Set struct {
	byProject map[string]*Override
	// Files records where each came from, so a report can name the file a
	// reader has to open.
	Files map[string]string
}

// LoadDir reads every *.hcl in dir.
//
// A directory with none is not an error — during the migration most projects
// are still unified diffs, and a tree that has not been converted yet must
// keep building.
func LoadDir(dir string) (*Set, error) {
	s := &Set{byProject: map[string]*Override{}, Files: map[string]string{}}
	if dir == "" {
		return s, nil
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.hcl"))
	if err != nil {
		return nil, fmt.Errorf("overrides: %w", err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		o, err := Parse(src, filepath.Base(p))
		if err != nil {
			return nil, err
		}
		if prev, dup := s.Files[o.Project]; dup {
			// Two files claiming one project would apply in whatever order the
			// glob happened to return, which is a fact about the filesystem
			// rather than about the recipes.
			return nil, fmt.Errorf("%s and %s both override %s",
				prev, filepath.Base(p), o.Project)
		}
		s.byProject[o.Project] = o
		s.Files[o.Project] = filepath.Base(p)
	}
	return s, nil
}

// Projects lists what the set overrides, sorted.
func (s *Set) Projects() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.byProject))
	for p := range s.byProject {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// For returns the override for a project, or nil.
func (s *Set) For(project string) *Override {
	if s == nil {
		return nil
	}
	return s.byProject[project]
}

// ApplyTo runs a project's override against a recipe document, if there is
// one. A project with no override is left exactly as it was.
func (s *Set) ApplyTo(project string, doc map[string]any) ([]Result, error) {
	o := s.For(project)
	if o == nil {
		return nil, nil
	}
	res, err := Apply(doc, o.Ops)
	if err != nil {
		return res, fmt.Errorf("%s: %w", s.Files[project], err)
	}
	return res, nil
}
