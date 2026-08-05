package buildscript

import (
	_ "embed"
	"os"
	"path/filepath"
)

// fixShebangs is the embedded fix-shebangs.ts build shim: a dependency-free
// POSIX-sh port of pkgx brewkit's libexec/fix-shebangs.ts. Many recipes
// (ruby-lang.org, gnu.org/stow, gnu.org/autoconf, exiftool.org, …) exec it by
// filename to make installed scripts' shebangs relocatable.
//
//go:embed libexec/fix-shebangs.ts
var fixShebangs []byte

// shims maps each build-shim helper's filename to its embedded contents. It is
// the single place to add further brewkit libexec helpers bk chooses to satisfy.
var shims = map[string][]byte{
	"fix-shebangs.ts": fixShebangs,
}

// os seams so WriteLibexec's error branches are testable.
var (
	mkdirAll  = os.MkdirAll
	writeFile = os.WriteFile
)

// WriteLibexec materialises the build-shim helpers into dir, each executable
// (0755). Point WrapOptions.BrewkitPath at dir so wrapper.go prepends it to the
// build PATH and recipes can exec the shims (e.g. `fix-shebangs.ts bin/*`).
func WriteLibexec(dir string) error {
	if err := mkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range shims {
		if err := writeFile(filepath.Join(dir, name), data, 0o755); err != nil {
			return err
		}
	}
	return nil
}
