// Package target is the single source of truth for the *target* platform/arch
// of a build.
//
// brewkit is historically a *native* build tool: everything pivots on the host
// == the machine it runs on == the thing it builds for. To cross-compile
// (notably Windows PE bottles built on a linux/darwin host) we split "where am
// I running" (Host) from "what am I building for" (Target). Only OUTPUT/target
// decisions consult Target:
//
//   - the `if:` guards in a recipe's build/test script
//   - platform-keyed env selection
//   - fix-up (POSIX relocation) selection
//   - the bottle slug (platform-key)
//
// Shell-environment decisions ("which TMPDIR does *this* host use") keep using
// Host. The target is taken from (first match wins):
//
//  1. the BREWKIT_TARGET env var  (eg. "windows/x86-64" or "windows+x86-64")
//  2. Host()  (native build — byte-for-byte unchanged behaviour)
package target

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Target is the resolved build target.
type Target struct {
	Platform string // darwin | linux | windows
	Arch     string // x86-64 | aarch64 | s390x
	// Triple is the compiler/host triple. For Windows this is the llvm-mingw
	// cross triple; otherwise the native triple.
	Triple string
}

// Cross reports whether Platform/Arch differ from the running host.
func (t Target) Cross() bool {
	h := Host()
	return t.Platform != h.Platform || t.Arch != h.Arch
}

// Slug is the pkgx bottle platform key, eg. "linux/x86-64".
func (t Target) Slug() string { return t.Platform + "/" + t.Arch }

var (
	supportedPlatforms = map[string]bool{"darwin": true, "linux": true, "windows": true}
	// s390x joined this list when a LinuxONE runner came online. An arch with
	// no machine behind it is a guess, and this is the one place that should
	// not hold one.
	//
	// Host() has never had a list -- it passes GOARCH through pkgxArch -- so
	// until now the same architecture was legal natively and refused the
	// moment a build NAMED it. That asymmetry is how `--platform linux/s390x`
	// reached
	//
	//	factory: unsupported target arch: "s390x"
	//
	// on a runner that had already checked the repo out and installed bk.
	supportedArches = map[string]bool{"x86-64": true, "aarch64": true, "s390x": true}
	// llvm.org/mingw-w64 cross triples (see the go-pkgx/packages Windows
	// factories: x86_64-w64-mingw32-clang / aarch64-w64-mingw32-clang).
	windowsTriples = map[string]string{
		"x86-64":  "x86_64-w64-mingw32",
		"aarch64": "aarch64-w64-mingw32",
	}
	overrideRE = regexp.MustCompile(`^([a-z0-9_]+)[/+]([a-z0-9-]+)$`)
)

// pkgxArch maps a Go GOARCH to pkgx's arch naming.
func pkgxArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86-64"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

// nativeTriple is a best-effort native compiler triple for the running host.
func nativeTriple() string { return nativeTripleFor(runtime.GOOS, runtime.GOARCH) }

// nativeTripleFor is the pure, testable core of nativeTriple.
func nativeTripleFor(goos, goarch string) string {
	arch := goarch
	switch goarch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	switch goos {
	case "darwin":
		return arch + "-apple-darwin"
	case "windows":
		return arch + "-w64-mingw32"
	default:
		return arch + "-unknown-linux-gnu"
	}
}

// Host is the platform/arch/triple of the machine bk is running on.
func Host() Target {
	p := runtime.GOOS
	return Target{Platform: p, Arch: pkgxArch(runtime.GOARCH), Triple: nativeTriple()}
}

// Override parses & validates BREWKIT_TARGET, returning ok=false when unset.
// It returns an error for a malformed or unsupported value.
func Override() (platform, arch string, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv("BREWKIT_TARGET"))
	if raw == "" {
		return "", "", false, nil
	}
	m := overrideRE.FindStringSubmatch(raw)
	if m == nil {
		return "", "", false, fmt.Errorf("invalid BREWKIT_TARGET: %q (expected eg. 'windows/x86-64')", raw)
	}
	platform, arch = m[1], m[2]
	if !supportedPlatforms[platform] {
		return "", "", false, fmt.Errorf("unsupported target platform: %q", platform)
	}
	if !supportedArches[arch] {
		// amd64 and arm64 are Go's spellings for two of these machines and the
		// mistake actually made, since every other arch name here is shared.
		// Refusing without saying so sends the reader looking for a missing
		// port rather than at the two characters that differ.
		if pkgx := pkgxArch(arch); pkgx != arch && supportedArches[pkgx] {
			return "", "", false, fmt.Errorf("unsupported target arch: %q (pkgx spells it %q)", arch, pkgx)
		}
		return "", "", false, fmt.Errorf("unsupported target arch: %q", arch)
	}
	return platform, arch, true, nil
}

// IsCross reports whether a BREWKIT_TARGET override is set (a cross build).
func IsCross() (bool, error) {
	_, _, ok, err := Override()
	return ok, err
}

// Resolve returns the build target, falling back to Host() for native builds.
func Resolve() (Target, error) {
	platform, arch, ok, err := Override()
	if err != nil {
		return Target{}, err
	}
	if !ok {
		return Host(), nil
	}
	triple := nativeTriple()
	if platform == "windows" {
		// This used to read `triple = windowsTriples[arch]` under a comment
		// saying every supported arch had an entry, so the lookup was total.
		// Widening supportedArches ended that, and a map miss does not fail:
		// it yields "", and a build carries an empty triple to whichever step
		// first needs a compiler, which is a long way from here.
		t, ok := windowsTriples[arch]
		if !ok {
			return Target{}, fmt.Errorf("no windows cross triple for arch %q (llvm-mingw targets %s)", arch, tripleArches())
		}
		triple = t
	}
	return Target{Platform: platform, Arch: arch, Triple: triple}, nil
}

// tripleArches lists the arches llvm-mingw can be targeted at, for the error
// above. Read from the table rather than restated, so it cannot describe a
// table it no longer matches.
func tripleArches() string {
	out := make([]string, 0, len(windowsTriples))
	for a := range windowsTriples {
		out = append(out, a)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
