// Command bk is the pure-Go brewkit — a from-scratch reimplementation of
// pkgx's build tool that is Target≠Host by design (first-class cross-compile,
// notably Windows PE bottles built on a linux/darwin host).
//
// This entrypoint currently exposes the pieces implemented so far:
//
//	bk target            print the resolved build target (honours BREWKIT_TARGET / --platform)
//	bk fixup <prefix>    run the post-build relocatability fix-ups on an install prefix
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/target"
)

var osExit = os.Exit

func main() { osExit(run(os.Args[1:], os.Stdout, os.Stderr)) }

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
		fmt.Fprintln(stderr, "usage: bk [--platform p] <target|fixup> [args]")
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
	case "build":
		return runBuild(rest[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "unknown command:", rest[0])
		return 2
	}
}
