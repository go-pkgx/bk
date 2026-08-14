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
	"fix-shebangs.ts":        {},
	"bkpyvenv":               {},
	"python-venv.sh":         {},
	"python-venv.py":         {},
	"python-venv-stubber.sh": {},
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
// compilerShims are materialised ON TOP of shims in pkgx-libc mode. They matter
// because a recipe may call `cc` or `gcc` DIRECTLY, ignoring $CC — sqlite's
// autosetup does exactly that ("No working C compiler found. Tried cc and
// gcc") — and the bare compiler has none of the sysroot/crt/runtime flags the
// sovereign mode depends on. Named first on PATH, these shims re-exec the real
// driver with them.
var compilerShims = []string{"cc", "gcc", "c++", "g++"}

// WriteLibexecFor is WriteLibexec plus the compiler shims when libcPkgx is set.
func WriteLibexecFor(dir string, libcPkgx bool) error {
	if err := WriteLibexec(dir); err != nil {
		return err
	}
	if !libcPkgx {
		return nil
	}
	self, err := osExecutable()
	if err != nil {
		return err
	}
	for _, name := range compilerShims {
		link := filepath.Join(dir, name)
		_ = osRemove(link)
		if err := osSymlink(self, link); err != nil {
			return err
		}
	}
	return nil
}

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
