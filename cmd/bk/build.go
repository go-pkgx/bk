package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/go-pkgx/bk/bottlepkg"
	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bk/fetch"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
	"github.com/go-pkgx/bottle"
)

// buildFactory builds a wired Runner; overridden in tests.
var buildFactory = realBuildRunner

// pickVersion adapts bottle.PickVersion (which returns a Ver) to the Runner's
// string-version contract.
func pickVersion(project, constraint string) (string, error) {
	v, err := bottle.PickVersion(project, constraint)
	return v.Raw, err
}

// runBash executes the generated build script under the sanitized env.
func runBash(scriptPath string, env []string) error {
	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// realBuildRunner wires the real bk/bottle implementations into a Runner. Every
// field but pickVersion/runBash is a direct package function of matching shape.
func realBuildRunner(pkgxBin string) *build.Runner {
	return &build.Runner{
		PickVersion: pickVersion,
		Fetch:       fetch.Fetch,
		FetchGit:    fetch.FetchGit,
		Touch:       build.TouchAutotools,
		Run:         runBash,
		FixUp:       fixup.FixUp,
		WriteBottle: bottlepkg.WriteBottle,
		ResolveDep:  pickVersion,
		PkgxBin:     pkgxBin,
		BashPath:    "/bin/bash",
	}
}

// runBuild is the `bk build` subcommand: read a recipe, resolve the target, and
// run the full pipeline (fetch → deps → generate → run → fix-up → package).
func runBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dist := fs.String("dist", "", "output dir for the packaged bottle (omit to skip packaging)")
	recipe := fs.String("recipe", "", "path to the package.yml recipe (required)")
	pkgx := fs.String("pkgx", "pkgx", "path to the pkgx binary used for the deps env")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if *recipe == "" || len(rest) < 1 {
		fmt.Fprintln(stderr, "usage: bk build --recipe <package.yml> [--dist <out>] <project>")
		return 2
	}
	project := rest[0]

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

	res, err := buildFactory(*pkgx).Build(rec, project, "*", tgt, target.Host(), *dist)
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
