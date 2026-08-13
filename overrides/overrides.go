// Package overrides applies the factory's local recipe-override patches to a
// pantry checkout, in pure Go — no `git apply` (or any other CLI) shell-out.
//
// An override is a plain `git diff` against the pantry root (paths of the form
// projects/<project>/package.yml): a candidate fix for a genuine upstream-recipe
// bug that the factory validates locally before it is proposed upstream to
// pkgxdev/pantry — the override file *is* the future pull request.
//
// Application is idempotent: every file a patch touches is first reset to its
// committed state, so a re-run against a persistent clone (the local repro
// harness mounts one) applies as cleanly as a fresh CI clone. A patch that no
// longer applies (upstream moved) is skipped loudly, never fatally — the recipe
// then simply falls back to upstream as-is.
package overrides

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	git "github.com/go-git/go-git/v5"
)

// Seams: the filesystem calls are indirected so every error branch is
// unit-testable without contriving an unwritable filesystem.
var (
	osReadFile  = os.ReadFile
	osWriteFile = os.WriteFile
	osRemove    = os.Remove
	osChmod     = os.Chmod
	osMkdirAll  = os.MkdirAll
)

// Options configures Apply.
type Options struct {
	Dir  string       // directory holding the *.patch overrides
	Root string       // pantry checkout to patch (a git worktree)
	Log  func(string) // progress lines (applied patches); optional
	Warn func(string) // problem lines (skipped patches); optional
}

func (o Options) log(format string, a ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(format, a...))
	}
}

func (o Options) warn(format string, a ...any) {
	if o.Warn != nil {
		o.Warn(fmt.Sprintf(format, a...))
	}
}

// Result reports the outcome, patch basenames in application order.
type Result struct {
	Applied []string
	Skipped []string
}

// Apply applies every Dir/*.patch to the pantry checkout at Root, in sorted
// (deterministic) order. It returns an error only when the override directory
// itself cannot be enumerated; individual patches that fail to parse or apply
// are reported through Warn and counted in Result.Skipped.
func Apply(o Options) (Result, error) {
	var res Result
	paths, err := filepath.Glob(filepath.Join(o.Dir, "*.patch"))
	if err != nil {
		return res, fmt.Errorf("overrides: %w", err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return res, nil
	}

	type patch struct {
		name  string
		files []*gitdiff.File
	}
	var parsed []patch
	var touched []string
	for _, p := range paths {
		name := filepath.Base(p)
		data, err := osReadFile(p)
		if err != nil {
			o.warn("override SKIP (unreadable): %s: %v", name, err)
			res.Skipped = append(res.Skipped, name)
			continue
		}
		files, _, err := gitdiff.Parse(bytes.NewReader(data))
		if err != nil {
			o.warn("override SKIP (unparsable): %s: %v", name, err)
			res.Skipped = append(res.Skipped, name)
			continue
		}
		if len(files) == 0 {
			o.warn("override SKIP (no file diffs): %s", name)
			res.Skipped = append(res.Skipped, name)
			continue
		}
		parsed = append(parsed, patch{name, files})
		touched = append(touched, pathsOf(files)...)
	}

	// Reset just the files these patches touch (fast and precise — the pantry is
	// a ~2k-project checkout), so a second run patches pristine sources instead
	// of already-patched ones.
	if len(touched) > 0 {
		if err := resetFiles(o.Root, dedupe(touched)); err != nil {
			o.warn("overrides: cannot reset pantry files: %v", err)
		}
	}

	for _, p := range parsed {
		if err := applyFiles(o.Root, p.files); err != nil {
			o.warn("override SKIP (does not apply): %s: %v", p.name, err)
			res.Skipped = append(res.Skipped, p.name)
			continue
		}
		o.log("override applied: %s", p.name)
		res.Applied = append(res.Applied, p.name)
	}
	return res, nil
}

// pathsOf lists every pantry-relative path a file diff reads or writes.
func pathsOf(files []*gitdiff.File) []string {
	var out []string
	for _, f := range files {
		for _, n := range []string{f.OldName, f.NewName} {
			if n != "" {
				out = append(out, strip(n))
			}
		}
	}
	return out
}

// strip removes the a/ or b/ prefix git puts on diff paths. go-gitdiff strips it
// from `diff --git` headers itself but leaves it on a bare unified diff, so a
// hand-written override still resolves against the pantry root (`git apply -p1`).
func strip(name string) string {
	for _, p := range []string{"a/", "b/"} {
		if rest, ok := strings.CutPrefix(name, p); ok {
			return rest
		}
	}
	return name
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// resetFiles restores the given repo-relative paths from HEAD, discarding any
// earlier override application (`git checkout -- <paths>`, in go-git).
func resetFiles(root string, files []string) error {
	repo, err := git.PlainOpen(root)
	if err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	return wt.Reset(&git.ResetOptions{Mode: git.HardReset, Files: files})
}

// change is one pending write, staged so a multi-file patch never lands
// half-applied: every fragment must apply before anything touches the disk.
type change struct {
	path   string
	data   []byte
	mode   fs.FileMode
	chmod  bool // the diff declares a mode → force it, even on an existing file
	remove bool
}

func applyFiles(root string, files []*gitdiff.File) error {
	changes := make([]change, 0, len(files))
	for _, f := range files {
		name := f.NewName
		if name == "" {
			name = f.OldName
		}
		if name == "" {
			return errors.New("file diff names no path")
		}
		dst := filepath.Join(root, filepath.FromSlash(strip(name)))
		if f.IsDelete {
			changes = append(changes, change{path: dst, remove: true})
			continue
		}

		var src []byte
		if !f.IsNew {
			b, err := osReadFile(filepath.Join(root, filepath.FromSlash(strip(f.OldName))))
			if err != nil {
				return err
			}
			src = b
		}

		// Mode only matters when the file is created (os.WriteFile leaves an
		// existing file's permissions alone, which is what we want) or when the
		// diff explicitly changes it.
		mode := fs.FileMode(0o644)
		if f.NewMode != 0 {
			mode = f.NewMode.Perm()
		}

		var buf bytes.Buffer
		if err := gitdiff.Apply(&buf, bytes.NewReader(src), f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		changes = append(changes, change{path: dst, data: buf.Bytes(), mode: mode, chmod: f.NewMode != 0})
	}

	for _, c := range changes {
		if c.remove {
			if err := osRemove(c.path); err != nil {
				return err
			}
			continue
		}
		if err := osMkdirAll(filepath.Dir(c.path), 0o755); err != nil {
			return err
		}
		if err := osWriteFile(c.path, c.data, c.mode); err != nil {
			return err
		}
		if c.chmod {
			if err := osChmod(c.path, c.mode); err != nil {
				return err
			}
		}
	}
	return nil
}
