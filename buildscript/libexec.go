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

// compilerShimsFor is compilerShims plus the TRIPLE-PREFIXED spellings, which
// autoconf reaches for before the bare ones.
//
// libisl's configure looks for `x86_64-pc-linux-gnu-gcc`, finds the bare gcc
// the gnu.org/gcc bottle puts on PATH — without any of the sovereign sysroot,
// crt or runtime flags — and stops at
//
//	checking whether the C compiler works... no
//	configure: error: C compiler cannot create executables
//
// Both spellings are made, and the reason is worth keeping: bk's own
// target.Triple says `x86_64-unknown-linux-gnu`, while autoconf's config.guess
// says `x86_64-pc-linux-gnu`, which is the one the failing build asked for.
// Deriving the name from our constant alone would have shimmed a name nobody
// looks up. They are symlinks; making both costs nothing and guessing wrong
// costs a build.
func compilerShimsFor(triple, platform, arch string) []string {
	names := append([]string{}, compilerShims...)
	seen := map[string]bool{}
	for _, t := range []string{triple, configGuessTriple(platform, arch)} {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		for _, base := range compilerShims {
			names = append(names, t+"-"+base)
		}
	}
	return names
}

// configGuessTriple is what autoconf's config.guess reports for a platform,
// which is not always what a toolchain calls itself.
func configGuessTriple(platform, arch string) string {
	if platform != "linux" {
		return ""
	}
	switch arch {
	case "x86-64", "amd64", "x86_64":
		return "x86_64-pc-linux-gnu"
	case "aarch64", "arm64":
		return "aarch64-unknown-linux-gnu"
	}
	return ""
}

// WriteLibexecFor is WriteLibexec plus the compiler shims when libcPkgx is set.
func WriteLibexecFor(dir string, libcPkgx bool, triple, platform, arch string) error {
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
	for _, name := range compilerShimsFor(triple, platform, arch) {
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
