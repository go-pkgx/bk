package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-pkgx/bk/bottlepkg"
	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/fetch"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bk/versions"
	"github.com/go-pkgx/bottle"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// buildFactory builds a wired Runner; overridden in tests.
var buildFactory = realBuildRunner

// lookPath resolves a binary name to an absolute path; a seam for tests.
var lookPath = exec.LookPath

// newInterp constructs the pure-Go shell interpreter; a seam so runBash's
// constructor-error branch is testable.
var newInterp = interp.New

// pickVersion adapts bottle.PickVersion (which returns a Ver) to the Runner's
// string-version contract.
func pickVersion(project, constraint string) (string, error) {
	v, err := bottle.PickVersion(project, constraint)
	return v.Raw, err
}

// runBash executes the generated build script under the sanitized env using
// mvdan.cc/sh's pure-Go interpreter — no `/bin/bash` binary. External build
// tools (gcc/make/cmake/pkgx) are still exec'd (that is the pkgx toolchain, not
// a shell dependency); only the shell language itself runs in-process.
func runBash(scriptPath string, env []string) error {
	return runBashTo(os.Stdout, os.Stderr)(scriptPath, env)
}

// runBashTo is runBash with the script's streams redirected. The factory points
// both at one writer (as factory.sh's `2>&1` did) so a failed build's error tail
// can be captured verbatim into failures-detail.txt.
func runBashTo(out, errOut io.Writer) func(string, []string) error {
	return func(scriptPath string, env []string) error {
		f, err := os.Open(scriptPath)
		if err != nil {
			return err
		}
		defer f.Close()
		prog, err := syntax.NewParser().Parse(f, scriptPath)
		if err != nil {
			return fmt.Errorf("parse %s: %w", scriptPath, err)
		}
		r, err := newInterp(
			interp.Env(expand.ListEnviron(env...)),
			interp.StdIO(os.Stdin, out, errOut),
		)
		if err != nil {
			return err
		}
		return r.Run(context.Background(), prog)
	}
}

// realBuildRunner wires the real bk/bottle implementations into a Runner. Every
// field but pickVersion/runBash is a direct package function of matching shape.
func realBuildRunner(pkgxBin string) *build.Runner {
	return &build.Runner{
		PickVersion:    pickVersion,
		ResolveVersion: versions.Resolve,
		Fetch:          fetch.Fetch,
		FetchGit:       fetch.FetchGit,
		Touch:          build.TouchAutotools,
		Run:            runBash,
		FixUp:          fixup.FixUp,
		WriteBottle:    bottlepkg.WriteBottle,
		ResolveDep:     pickVersion,
		PkgxBin:        pkgxBin,
		BashPath:       "/bin/bash",
	}
}

// runBuild is the `bk build` subcommand: read a recipe, resolve the target, and
// run the full pipeline (fetch → deps → generate → run → fix-up → package).
func runBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dist := fs.String("dist", "", "output dir for the packaged bottle (omit to skip packaging)")
	recipe := fs.String("recipe", "", "path to the package.yml recipe (required)")
	version := fs.String("version", "", "exact version to build (default: latest resolvable)")
	pkgx := fs.String("pkgx", "pkgx", "path to the pkgx binary used for the deps env")
	libc := fs.String("libc", "", `C library to link against: "pkgx" targets the gnu.org/glibc bottle (sovereign FROM-scratch, linux, C recipes); default keeps the build container's system glibc`)
	glibc := fs.String("glibc", "", `with --libc=pkgx, pin the exact glibc version to link against, e.g. -glibc 2.27.0 (a chosen HPC floor); default newest`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if *recipe == "" || len(rest) < 1 {
		fmt.Fprintln(stderr, "usage: bk build --recipe <package.yml> [--dist <out>] <project>")
		return 2
	}
	project := rest[0]

	// Resolve the pkgx binary to an absolute path: the build script runs under a
	// sanitized PATH (/usr/bin:/bin:/usr/sbin:/sbin) that excludes /usr/local/bin
	// and ~/.local/bin, so a bare "pkgx" would not be found for the deps eval. If
	// resolution fails, keep the value as given (explicit paths still work and the
	// original error stays clear).
	pkgxBin := *pkgx
	if abs, err := lookPath(pkgxBin); err == nil {
		pkgxBin = abs
	}

	runner := buildFactory(pkgxBin)
	runner.RecipeDir = filepath.Dir(*recipe)
	runner.LibcMode = *libc
	runner.Glibc = *glibc

	data, err := os.ReadFile(*recipe)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rec, err := pantry.Parse(data)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	tgt, err := target.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	// A pinned --version becomes an exact `=` semver constraint so the resolver
	// selects precisely that candidate; empty keeps the default "*" (latest).
	constraint := "*"
	if *version != "" {
		constraint = "=" + *version
	}

	res, err := runner.Build(rec, project, constraint, tgt, target.Host(), *dist)
	if err != nil {
		fmt.Fprintln(stderr, "build failed:", err)
		return 1
	}
	fmt.Fprintf(stdout, "built %s %s → %s\n", project, res.Version, res.Install)
	if res.BottlePath != "" {
		fmt.Fprintln(stdout, "bottle:", res.BottlePath)
	}
	return 0
}
