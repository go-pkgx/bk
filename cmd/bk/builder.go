package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-pkgx/bottle"
)

// runBuilder implements `bk builder`: stage a FROM-scratch build environment as
// a DIRECTORY — a pkgx closure, the loader and /bin/sh posed at their canonical
// paths, stubs in /usr/bin, and the bk/pkgx binaries that drive a build.
//
// The image the sovereign pilot used was assembled by a Containerfile: a
// `FROM scratch` stage whose RUN step let pkgm install the toolchain from
// inside the image. That works, but it needs a container builder to exist and
// to be running the target's architecture, and it produces only an image —
// while the thing that actually consumes a builder rootfs, a micro-VM, boots a
// DIRECTORY (weft shares it over virtio-fs; an OCI image is unpacked back into
// a directory before boot). So the directory is the primitive, and this
// subcommand produces it directly, in Go, on any host: bottle.InstallFor and
// bottle.StubBinsStaged already know how to materialise another platform's
// userland.
//
// Nothing here needs root, docker, or a linux machine.
func runBuilder(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("builder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "directory to stage the rootfs in (required)")
	platform := fs.String("platform", envOr("PLATFORM", "linux/aarch64"), "target os/arch")
	toolchain := fs.String("toolchain", "", "file listing the toolchain packages, one `project[constraint]` per line (default: the built-in list)")
	bkBin := fs.String("bk", "", "path to a target-arch bk binary to install at /usr/local/bin/bk")
	pkgxBin := fs.String("pkgx", "", "path to a target-arch pkgx binary to install at /usr/local/bin/pkgx")
	dist := fs.String("dist", "", "bottle registry to install from (default $PKGX_DIST)")
	overlay := fs.String("overlay", "", "pantry overlay consulted before the upstream pantry (default $PKGX_PANTRY_OVERLAY)")
	microvm := fs.Bool("microvm", false, "also write .weft-microvm/config.json so `weft microvm run` can boot the directory as-is")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(stderr, "usage: bk builder --out <dir> [--platform linux/aarch64] [--toolchain file]")
		return 2
	}
	osn, arch, _ := strings.Cut(*platform, "/")
	if osn != "linux" {
		fmt.Fprintf(stderr, "builder: %s images are not a thing — a scratch rootfs is a linux notion\n", osn)
		return 2
	}
	if *dist != "" {
		bottle.DistBase = strings.TrimRight(*dist, "/")
	}
	if *overlay != "" {
		bottle.PantryOverlay = strings.TrimRight(*overlay, "/")
	}

	roots, err := toolchainRoots(*toolchain)
	if err != nil {
		fmt.Fprintln(stderr, "builder:", err)
		return 1
	}

	if err := stageBuilder(stageOptions{
		Root:    *out,
		OS:      osn,
		Arch:    arch,
		Roots:   roots,
		BkBin:   *bkBin,
		PkgxBin: *pkgxBin,
		MicroVM: *microvm,
		Overlay: bottle.PantryOverlay,
		Dist:    bottle.DistBase,
		Log:     func(s string) { fmt.Fprintln(stdout, s) },
	}); err != nil {
		fmt.Fprintln(stderr, "builder:", err)
		return 1
	}
	return 0
}

// guestPkgxDir is where the staged bottles live once the rootfs is the root:
// the stubs, the loader symlinks and PKGX_DIR must all agree on it.
const guestPkgxDir = "/pkgx"

// defaultToolchain is the build environment of the sovereign builder. It
// mirrors go-pkgx/packages' builder/toolchain.txt; --toolchain overrides it.
// Each entry earns its place — a toolchain nobody can explain is one nobody
// dares trim.
var defaultToolchain = []string{
	"llvm.org",                 // clang, lld, compiler-rt — the compiler itself
	"gnu.org/glibc",            // libc, crt objects and the dynamic loader
	"kernel.org/linux-headers", // glibc's headers include <linux/limits.h>
	"gnu.org/binutils",         // ar/ranlib: the llvm bottle ships llvm-ar, not `ar`
	"gnu.org/make",             // the build driver most recipes use
	"gnu.org/bash",             // `make` runs every recipe line through /bin/sh
	"gnu.org/coreutils",        // mkdir, install, ln… the vocabulary of a Makefile
	"gnu.org/sed",
	"gnu.org/grep",
	"gnu.org/gawk",      // config-header generation
	"gnu.org/m4",        // autoconf's macro processor
	"gnu.org/findutils", // find + xargs, used by recipes at install time
	"gnu.org/diffutils", // `cmp`, called by recipes' own test steps
	"gnu.org/patch",     // recipes that patch their own sources
	"gnu.org/autoconf",
	"gnu.org/automake",
	"gnu.org/libtool",
	"gnu.org/bison",
	"github.com/westes/flex",
	"gnu.org/pkg-config",
	"perl.org",         // openssl and friends drive their build with it
	"gnu.org/help2man", // generated man pages during `make install`
	"curl.se/ca-certs", // a scratch image has no trust store
}

// toolchainRoots reads the package list, or returns the built-in one. Lines are
// `project[constraint]`, `#` starts a comment, blanks are ignored — the same
// shape pkgm's -f file uses, so one list feeds both.
func toolchainRoots(path string) (map[string]string, error) {
	lines := defaultToolchain
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		lines = strings.Split(string(b), "\n")
	}
	roots := map[string]string{}
	for _, ln := range lines {
		if i := strings.IndexByte(ln, '#'); i >= 0 {
			ln = ln[:i]
		}
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		proj, constraint := splitToolchainSpec(ln)
		roots[proj] = constraint
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("toolchain list is empty")
	}
	return roots, nil
}

// splitToolchainSpec splits `project[@^~<>=constraint]` into its two halves,
// defaulting to "*". The delimiters match pkgx's own spec syntax.
func splitToolchainSpec(s string) (project, constraint string) {
	if i := strings.IndexAny(s, "@^~<>="); i > 0 {
		c := strings.TrimPrefix(s[i:], "@")
		if c == "" {
			c = "*"
		}
		return s[:i], c
	}
	return s, "*"
}

// stageOptions is the whole input of a staging run.
type stageOptions struct {
	Root     string
	OS, Arch string
	Roots    map[string]string
	BkBin    string
	PkgxBin  string
	MicroVM  bool
	Overlay  string
	Dist     string
	Log      func(string)
}

// stageBuilder materialises the rootfs. Order matters: the closure has to be
// on disk before the loader can be found in it, and the loader has to be posed
// before a stub is worth writing.
func stageBuilder(o stageOptions) error {
	pkgxDir := filepath.Join(o.Root, "pkgx")
	o.Log(fmt.Sprintf("builder: resolving the toolchain closure for %s/%s", o.OS, o.Arch))
	closure, err := resolveClosureFor(o.Roots, o.OS, o.Arch)
	if err != nil {
		return fmt.Errorf("resolve closure: %w", err)
	}
	o.Log(fmt.Sprintf("builder: %d packages", len(closure)))

	for _, r := range closure {
		fresh, err := installFor(r, pkgxDir, o.OS, o.Arch)
		if err != nil {
			return fmt.Errorf("install %s %s: %w", r.Project, r.Version.Raw, err)
		}
		if fresh {
			o.Log(fmt.Sprintf("  + %s %s", r.Project, r.Version.Raw))
		}
	}

	// The loader and /bin/sh have to answer at their canonical absolute paths:
	// every bottle ELF names PT_INTERP=/lib/ld-linux-*, and make runs each
	// recipe line through /bin/sh. Both symlinks must point at GUEST paths.
	loader := findLoaderFor(pkgxDir, o.Arch)
	if loader == "" {
		return fmt.Errorf("no %s loader in the staged glibc — is gnu.org/glibc in the toolchain?", o.Arch)
	}
	shell := bottle.FindClosureBin(closure, pkgxDir, "gnu.org/bash", "bash")
	if err := bottle.SetupScratchRootfsAt(o.Root,
		bottle.LoaderNameFor(o.Arch),
		guestPath(loader, pkgxDir),
		guestPath(shell, pkgxDir),
	); err != nil {
		return fmt.Errorf("pose loader + /bin/sh: %w", err)
	}

	// /usr, not /usr/local: bk builds under a sanitised PATH that lists
	// /usr/bin and /bin but not /usr/local/bin, so a stub outside it is a stub
	// the build cannot call.
	n, err := stubBinsStaged(closure, bottle.Stage{
		Dir:      pkgxDir,
		GuestDir: guestPkgxDir,
		Prefix:   filepath.Join(o.Root, "usr"),
		OS:       o.OS,
		Arch:     o.Arch,
	})
	if err != nil {
		return fmt.Errorf("stub binaries: %w", err)
	}
	o.Log(fmt.Sprintf("builder: %d stubs in /usr/bin", n))

	// A build writes: give it the directories every toolchain assumes exist.
	// 1777 on the temp dirs, as everywhere else — a build that drops privileges
	// still has to be able to write there.
	for _, d := range []string{"tmp", "var/tmp", "root", "usr/local/bin"} {
		if err := os.MkdirAll(filepath.Join(o.Root, d), 0o755); err != nil {
			return err
		}
	}
	for _, d := range []string{"tmp", "var/tmp"} {
		// 0o1777 is NOT the sticky bit in Go: os.FileMode keeps it in a HIGH bit
		// (os.ModeSticky), so the octal literal a chmod(1) user reaches for
		// silently sets 0777 and drops the sticky.
		if err := osChmod(filepath.Join(o.Root, d), 0o777|os.ModeSticky); err != nil {
			return err
		}
	}

	for _, b := range []struct{ src, name string }{{o.BkBin, "bk"}, {o.PkgxBin, "pkgx"}} {
		if b.src == "" {
			continue
		}
		dst := filepath.Join(o.Root, "usr", "local", "bin", b.name)
		if err := copyExecutable(b.src, dst); err != nil {
			return fmt.Errorf("install %s: %w", b.name, err)
		}
		o.Log(fmt.Sprintf("builder: %s → /usr/local/bin/%s", b.src, b.name))
	}

	if o.MicroVM {
		if err := writeMicroVMConfig(o, closure, pkgxDir); err != nil {
			return fmt.Errorf("write .weft-microvm/config.json: %w", err)
		}
		o.Log("builder: .weft-microvm/config.json written — `weft microvm run` can boot this directory")
	}
	return nil
}

// guestPath rewrites a staged path to the one it will have once the rootfs is
// the root. Returns "" unchanged, so a missing optional (no bash in the
// closure) stays missing rather than becoming "/pkgx".
func guestPath(p, pkgxDir string) string {
	if p == "" {
		return ""
	}
	rel, err := filepath.Rel(pkgxDir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return filepath.Join(guestPkgxDir, rel)
}

// copyExecutable copies src to dst with the executable bit set.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := ioCopy(f, in); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := closeWritten(f); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// microVMConfig is the process spec weft-microvm-init reads from
// <rootfs>/.weft-microvm/config.json — the same shape `weft microvm pull`
// derives from an OCI image config. Writing it here is what lets a staged
// directory be booted with no registry and no image at all: weft's Pull treats
// the file as its "already materialised" sentinel and skips straight to boot.
type microVMConfig struct {
	Process microVMProcess `json:"process"`
}

type microVMProcess struct {
	Args []string    `json:"args"`
	Env  []string    `json:"env"`
	Cwd  string      `json:"cwd"`
	User microVMUser `json:"user"`
}

type microVMUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// writeMicroVMConfig records how to run bk inside the staged rootfs.
//
// LD_LIBRARY_PATH is spelled out rather than left to pkgx: in a container the
// image ENTRYPOINT ran bk THROUGH pkgx, which composed the environment at exec
// time, but a micro-VM's init execs the args verbatim. Without it the first
// thing that happens in the guest is /bin/sh failing to find libc.so.6 —
// measured, in exactly that way.
func writeMicroVMConfig(o stageOptions, closure []bottle.Resolved, pkgxDir string) error {
	libPath := strings.ReplaceAll(bottle.LibPath(closure, pkgxDir), pkgxDir, guestPkgxDir)
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LD_LIBRARY_PATH=" + libPath,
		"PKGX_DIR=" + guestPkgxDir,
		"HOME=/root",
		"TMPDIR=/tmp",
	}
	if o.Dist != "" {
		env = append(env, "PKGX_DIST="+o.Dist)
	}
	if o.Overlay != "" {
		env = append(env, "PKGX_PANTRY_OVERLAY="+o.Overlay)
	}
	sort.Strings(env)

	cfg := microVMConfig{Process: microVMProcess{
		Args: []string{"/usr/local/bin/bk"},
		Env:  env,
		Cwd:  "/",
	}}
	b, err := jsonMarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(o.Root, ".weft-microvm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), append(b, '\n'), 0o644)
}

// Seams. Staging talks to a registry over the network; the tests drive these
// instead, so a builder test proves the ORCHESTRATION — closure before
// install, install before loader, loader before stubs — rather than re-proving
// bottle's transport.
var (
	resolveClosureFor = bottle.ResolveClosureFor
	installFor        = bottle.InstallFor
	findLoaderFor     = bottle.FindLoaderFor
	stubBinsStaged    = bottle.StubBinsStaged
)

// More seams, for the branches a filesystem will not produce on demand: a
// write that fails only on Close (a full disk, a network filesystem) and an
// encoder that cannot encode. Both messages are the only thing a user would
// ever see of these paths, so they are worth exercising.
var (
	ioCopy            = io.Copy
	closeWritten      = func(f *os.File) error { return f.Close() }
	jsonMarshalIndent = json.MarshalIndent
	osChmod           = os.Chmod
)
