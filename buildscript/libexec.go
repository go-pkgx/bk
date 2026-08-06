package buildscript

import (
	"os"
	"path/filepath"
)

// shims is the set of build-shim helper NAMES bk materialises into the per-build
// libexec dir. Each is created as a symlink to the running bk binary: bk is a
// multi-call ("busybox-style") binary that, when invoked under one of these
// names, runs the matching pure-Go helper — so no shell interpreter is involved.
// Many recipes (ruby-lang.org, gnu.org/stow, gnu.org/autoconf, exiftool.org, …)
// exec these by filename, e.g. `fix-shebangs.ts bin/*`. It is the single place
// to add further brewkit libexec helpers bk chooses to satisfy.
var shims = map[string]struct{}{
	"fix-shebangs.ts": {},
}

// seams so WriteLibexec's error branches are testable.
var (
	mkdirAll     = os.MkdirAll
	osExecutable = os.Executable
	osSymlink    = os.Symlink
	osRemove     = os.Remove
)

// WriteLibexec materialises the build-shim helpers into dir, each as a symlink
// to the running bk executable. Point WrapOptions.BrewkitPath at dir so
// wrapper.go prepends it to the build PATH and recipes can exec the shims (e.g.
// `fix-shebangs.ts bin/*`); bk's multi-call dispatch then runs the pure-Go
// helper selected by argv[0]'s basename. If the running executable's path
// cannot be determined the error is returned (never silently fall back to a
// shell script — the failure must stay visible).
func WriteLibexec(dir string) error {
	if err := mkdirAll(dir, 0o755); err != nil {
		return err
	}
	self, err := osExecutable()
	if err != nil {
		return err
	}
	for name := range shims {
		link := filepath.Join(dir, name)
		_ = osRemove(link) // idempotent: os.Symlink fails if the path already exists
		if err := osSymlink(self, link); err != nil {
			return err
		}
	}
	return nil
}
