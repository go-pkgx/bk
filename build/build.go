// Package build holds the orchestration helpers that turn a parsed recipe into
// a runnable build: the dependency closure (recipe deps reduced for the target),
// the ambient base toolchain brewkit provides, a sanitized run environment, and
// the autotools maintainer-mode defeat. These were distilled from proving bk on
// real packages (zlib native, zlib→windows cross, wget with openssl).
package build

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-pkgx/bk/target"
)

var (
	platforms   = map[string]bool{"darwin": true, "linux": true, "windows": true}
	arches      = map[string]bool{"x86-64": true, "aarch64": true}
	osArchDepRE = regexp.MustCompile(`^(darwin|linux|windows)/(aarch64|x86-64)$`)
)

// DepSpecs reduces a recipe dependency map (project → constraint, with optional
// platform-keyed sub-maps) into pkgx pkgspecs ("project@constraint") for the
// target. Non-matching platform keys are dropped; matching ones are merged in.
// Output is sorted for determinism.
func DepSpecs(deps map[string]any, tgt target.Target) []string {
	flat := map[string]string{}
	for k, v := range deps {
		os, arch, isKey := platformKey(k)
		if !isKey {
			flat[k] = valStr(v)
			continue
		}
		if os != "" && os != tgt.Platform {
			continue
		}
		if arch != "" && arch != tgt.Arch {
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			for sk, sv := range sub {
				flat[sk] = valStr(sv)
			}
		}
	}
	specs := make([]string, 0, len(flat))
	for p, c := range flat {
		if c == "" || c == "*" {
			specs = append(specs, p)
		} else {
			specs = append(specs, p+"@"+c)
		}
	}
	sort.Strings(specs)
	return specs
}

func platformKey(k string) (os, arch string, ok bool) {
	if m := osArchDepRE.FindStringSubmatch(k); m != nil {
		return m[1], m[2], true
	}
	if platforms[k] {
		return k, "", true
	}
	if arches[k] {
		return "", k, true
	}
	return "", "", false
}

func valStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "*"
	case string:
		return x
	case bool:
		if x {
			return "*"
		}
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

// BaseToolchain is the ambient build toolset brewkit provides so recipes need
// only declare their SPECIFIC deps. Without it, autotools packages fail on
// aclocal/makeinfo not-found (proven with wget).
func BaseToolchain() []string {
	return []string{
		"gnu.org/autoconf", "gnu.org/automake", "gnu.org/libtool", "gnu.org/m4",
		"gnu.org/make", "gnu.org/gettext", "gnu.org/texinfo", "perl.org",
		"gnu.org/sed", "gnu.org/coreutils", "gnu.org/grep", "gnu.org/gawk",
		"freedesktop.org/pkg-config",
	}
}

// EvalDeps is the full `pkgx +…` set for a build: the base toolchain plus the
// recipe's own runtime + build deps, de-duplicated and sorted.
func EvalDeps(runtime, buildDeps map[string]any, tgt target.Target) []string {
	seen := map[string]bool{}
	var out []string
	add := func(specs []string) {
		for _, s := range specs {
			key := strings.SplitN(s, "@", 2)[0]
			if !seen[key] {
				seen[key] = true
				out = append(out, s)
			}
		}
	}
	add(BaseToolchain())
	add(DepSpecs(runtime, tgt))
	add(DepSpecs(buildDeps, tgt))
	sort.Strings(out)
	return out
}

// SanitizedEnv is the clean environment the build script runs within (brewkit's
// clearEnv): a fixed PATH, PKGX_DIR and HOME, plus a small allow-list passed
// through from the caller's environment. It prevents a stray toolchain (eg. a
// system Homebrew) from leaking into the build (proven: wget linked the wrong
// openssl under an unsanitized env).
func SanitizedEnv(home, pkgxDir string) []string {
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"PKGX_DIR=" + pkgxDir,
	}
	for _, k := range []string{"LANG", "LOGNAME", "USER", "TERM", "PKGX_PANTRY_DIR", "PKGX_PANTRY_PATH", "GITHUB_TOKEN"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// autotools file tiers, oldest → newest, so outputs end up newer than inputs.
var autotoolsTiers = [][]string{
	{".ac", ".am", "acinclude.m4"}, // inputs
	{"aclocal.m4"},
	{"config.h.in", "configure"},
	{"Makefile.in"}, // final outputs
}

// TouchAutotools defeats autotools maintainer-mode: an extracted tarball's file
// timestamps make `make` think configure.ac changed and rerun aclocal/automake
// (which fail without the exact tool versions). Touching the generated files
// newer than their sources, in ascending tiers, marks the build system fresh.
func TouchAutotools(srcDir string) error {
	base := time.Now().Add(-time.Hour)
	for tier, pats := range autotoolsTiers {
		when := base.Add(time.Duration(tier) * time.Minute)
		err := filepath.WalkDir(srcDir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			name := d.Name()
			for _, pat := range pats {
				if name == pat || (strings.HasPrefix(pat, ".") && strings.HasSuffix(name, pat)) {
					return osChtimes(p, when, when)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
