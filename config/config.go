// Package config computes the on-disk workspace layout for a build. The build
// tree is keyed on the *target* platform+arch so a cross build (eg. Windows)
// gets its own directories and never collides with a native build of the same
// package.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-pkgx/bk/target"
)

// userHomeDir is a seam so tests can exercise the no-home fallback paths.
var userHomeDir = os.UserHomeDir

// Paths is the set of directories a build uses.
type Paths struct {
	Home         string // a fresh HOME for the build (== bkroot for the default layout)
	Src          string // extracted sources
	Build        string // where the build runs
	Test         string // where tests stage and run
	TarballDir   string // where downloaded source tarballs are cached
	Install      string // final install prefix: $PKGX_DIR/<project>/v<version>
	BuildInstall string // staging prefix the build writes to: <Install>+brewing
}

// PkgxDir returns the pkgx prefix ($PKGX_DIR, else ~/.pkgx).
func PkgxDir() string {
	if d := os.Getenv("PKGX_DIR"); d != "" {
		return d
	}
	home, err := userHomeDir()
	if err != nil {
		return ".pkgx"
	}
	return filepath.Join(home, ".pkgx")
}

// dataHome is the XDG data home ($XDG_DATA_HOME, else ~/.local/share; on macOS
// ~/Library/Application Support), matching where brewkit stores its build tree.
func dataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, err := userHomeDir()
	if err != nil {
		return filepath.Join(".local", "share")
	}
	return dataHomeFor(runtime.GOOS, home)
}

// dataHomeFor is the pure, testable OS split of dataHome.
func dataHomeFor(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".local", "share")
}

// Compute lays out the build directories for project@version built for tgt.
//
// When PKGX_PANTRY_DIR (or the older PKGX_PANTRY_PATH) is set, the workspace is
// rooted under that checkout (homes/srcs/builds/testbeds), matching brewkit's
// in-pantry layout; otherwise it lives under the XDG data home, keyed on the
// target platform+arch.
func Compute(project, version string, tgt target.Target) Paths {
	install := filepath.Join(PkgxDir(), filepath.FromSlash(project), "v"+version)
	p := Paths{
		Install:      install,
		BuildInstall: install + "+brewing",
	}

	if checkout := pantryCheckout(); checkout != "" {
		slug := strings.ReplaceAll(project, "/", "__")
		spec := slug + "-" + version
		p.Home = filepath.Join(checkout, "homes", spec)
		p.Src = filepath.Join(checkout, "srcs", spec)
		p.Build = filepath.Join(checkout, "builds", spec)
		p.Test = filepath.Join(checkout, "testbeds", spec)
		p.TarballDir = filepath.Join(checkout, "srcs")
		return p
	}

	dh := filepath.Join(dataHome(), "brewkit")
	root := filepath.Join(dh, tgt.Platform+"+"+tgt.Arch, filepath.FromSlash(project), "v"+version)
	p.Home = root
	p.Src = filepath.Join(root, "src")
	p.Build = filepath.Join(root, "build")
	p.Test = filepath.Join(root, "testbed")
	p.TarballDir = dh
	return p
}

func pantryCheckout() string {
	if d := os.Getenv("PKGX_PANTRY_DIR"); d != "" {
		return d
	}
	return os.Getenv("PKGX_PANTRY_PATH")
}
