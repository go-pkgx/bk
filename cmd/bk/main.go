// Command bk is the pure-Go brewkit — a from-scratch reimplementation of
// pkgx's build tool that is Target≠Host by design (first-class cross-compile,
// notably Windows PE bottles built on a linux/darwin host).
//
// This entrypoint currently exposes the pieces implemented so far:
//
//	bk target            print the resolved build target (honours BREWKIT_TARGET / --platform)
//	bk fixup <prefix>    run the post-build relocatability fix-ups on an install prefix
//	bk versions          list a recipe's candidate versions
//	bk build             build a recipe into a bottle
//	bk publish           push a built bottle to an OCI registry
//	bk closure           print a project set's transitive runtime closure (topological)
//	bk factory           build a recipe set's whole closure and publish every bottle
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/target"
)

var osExit = os.Exit

func main() {
	// Multi-call ("busybox-style") dispatch: bk is materialised into a build's
	// libexec dir as symlinks named after brewkit shims. When a recipe execs one
	// (e.g. `fix-shebangs.ts bin/*`), argv[0] is the symlink path, so its
	// basename selects the pure-Go helper — no shell interpreter is involved.
	switch filepath.Base(os.Args[0]) {
	case "fix-shebangs.ts":
		osExit(fixShebangs(os.Args[1:], os.Stderr))
		return
	case "bkpyvenv":
		osExit(bkpyvenv(os.Args[1:], os.Stderr))
		return
	case "python-venv.sh":
		osExit(pythonVenvSh(os.Args[1:], os.Stderr))
		return
	case "python-venv.py":
		osExit(pythonVenvPy(os.Args[1:], os.Stderr))
		return
	case "python-venv-stubber.sh":
		osExit(pythonVenvStubber(os.Args[1:], os.Stderr))
		return
	}
	osExit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bk", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "cross-build target, eg. windows/x86-64 (also BREWKIT_TARGET)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *platform != "" {
		os.Setenv("BREWKIT_TARGET", *platform)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: bk [--platform p] <target|fixup|versions|build|publish|closure|factory> [args]")
		return 2
	}

	tgt, err := target.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	switch rest[0] {
	case "target":
		fmt.Fprintf(stdout, "%s/%s triple=%s cross=%v\n", tgt.Platform, tgt.Arch, tgt.Triple, tgt.Cross())
		return 0
	case "fixup":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: bk fixup <prefix>")
			return 2
		}
		err := fixup.FixUp(fixup.Options{
			Prefix:   rest[1],
			Platform: tgt.Platform,
			Log:      func(s string) { fmt.Fprintln(stdout, s) },
		})
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	case "versions":
		return runVersions(rest[1:], stdout, stderr)
	case "build":
		return runBuild(rest[1:], stdout, stderr)
	case "publish":
		return runPublish(rest[1:], stdout, stderr)
	case "closure":
		return runClosure(rest[1:], stdout, stderr)
	case "factory":
		return runFactory(rest[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "unknown command:", rest[0])
		return 2
	}
}
