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

	"github.com/go-pkgx/bk/moustache"
	"github.com/go-pkgx/bk/target"
)

var (
	platforms   = map[string]bool{"darwin": true, "linux": true, "windows": true}
	arches      = map[string]bool{"x86-64": true, "aarch64": true}
	osArchDepRE = regexp.MustCompile(`^(darwin|linux|windows)/(aarch64|x86-64)$`)
)

// reduceDepMap flattens a recipe dependency map (project → constraint, with
// optional platform-keyed sub-maps) to project → constraint for the target,
// dropping non-matching platform keys and merging matching ones.
func reduceDepMap(deps map[string]any, tgt target.Target) map[string]string {
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
	return flat
}

// DepSpecs reduces a recipe dependency map into pkgx pkgspecs
// ("project@constraint") for the target, sorted for determinism.
func DepSpecs(deps map[string]any, tgt target.Target) []string {
	flat := reduceDepMap(deps, tgt)
	specs := make([]string, 0, len(flat))
	for p, c := range flat {
		specs = append(specs, depSpec(p, c))
	}
	sort.Strings(specs)
	return specs
}

// depSpec renders a single project+constraint pair as a pkgx pkgspec. pkgx
// (v2.10.3) parses the operator forms differently: a range operator (^ ~ > <)
// is appended DIRECTLY to the project (`cmake.org^3`, `gnu.org/gmp>=6`); an
// exact `=X.Y.Z` and a bare numeric constraint use `@` (`foo@1.2.3`, `foo@3`);
// `*`/empty is a bare project. Appending `@` to a range operator
// (`cmake.org@^3`) makes pkgx reject it with "invalid semver".
func depSpec(project, constraint string) string {
	switch {
	case constraint == "" || constraint == "*":
		return project
	case strings.HasPrefix(constraint, "^"), strings.HasPrefix(constraint, "~"),
		strings.HasPrefix(constraint, ">"), strings.HasPrefix(constraint, "<"):
		return project + constraint
	case strings.HasPrefix(constraint, "="):
		return project + "@" + strings.TrimPrefix(constraint, "=")
	default:
		return project + "@" + constraint
	}
}

// specProject extracts the bare project name from a rendered pkgspec, stopping
// at the first version delimiter (@ or a range operator) so dedup keys match
// regardless of the constraint form.
func specProject(spec string) string {
	if i := strings.IndexAny(spec, "@^~<>="); i >= 0 {
		return spec[:i]
	}
	return spec
}

// DepTokens resolves each recipe dependency (runtime + build) to a version and
// install prefix ($PKGX_DIR/<project>/v<version>) and returns the
// `{{deps.<project>.prefix}}` / `{{deps.<project>.version.*}}` moustache tokens.
// resolve maps a project + constraint to a concrete version.
func DepTokens(runtime, buildDeps map[string]any, tgt target.Target, pkgxDir string, resolve func(project, constraint string) (string, error)) ([]moustache.Token, error) {
	flat := reduceDepMap(runtime, tgt)
	for p, c := range reduceDepMap(buildDeps, tgt) {
		flat[p] = c
	}
	projects := make([]string, 0, len(flat))
	for p := range flat {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	deps := make([]moustache.Dep, 0, len(projects))
	for _, p := range projects {
		ver, err := resolve(p, flat[p])
		if err != nil {
			return nil, err
		}
		deps = append(deps, moustache.Dep{
			Project: p,
			Version: ver,
			Path:    filepath.Join(pkgxDir, filepath.FromSlash(p), "v"+ver),
		})
	}
	return moustache.Deps(deps), nil
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
		"gnu.org/sed", "gnu.org/coreutils", "gnu.org/grep",
		// Pin gawk to 5.3: gawk 5.4.1 has a regression that silently mishandles
		// the option-resolution scripts autotools packages use to generate config
		// headers — e.g. libpng's pnglibconf generation drops PNG_SETJMP_SUPPORTED,
		// which then breaks the build (isolated: gawk 5.2.x and 5.3.2 keep it, 5.4.1
		// drops it under any gcc, in every awk mode). 5.3.2 is the newest gawk with
		// a working pkgx bottle; relax when a later 5.4.x fixes the regression.
		"gnu.org/gawk@5.3",
		// bison provides the yacc/bison grammar compiler. autotools packages that
		// ship a .y grammar (e.g. gettext's gettext-runtime/intl/plural.y) regenerate
		// the .c from it during the build via ylwrap; without bison that step fails
		// with "bison: command not found" (Error 127). Provides bin/bison + bin/yacc.
		"gnu.org/bison",
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
			key := specProject(s)
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
		// Neutralise autotools maintainer-mode. A release tarball ships a correct
		// configure/Makefile.in/aclocal.m4, but timestamp skew after extraction
		// makes `make` try to regenerate them with a pinned aclocal-1.NN /
		// automake-1.NN that isn't installed (→ "aclocal-1.18: command not
		// found", exit 127). Passing these as make variable overrides via
		// MAKEFLAGS no-ops every regen rule, so the shipped generated files are
		// used as-is. (make-only; a recipe's own direct autoreconf is untouched.)
		//
		// MAKEINFO is deliberately NOT no-op'd: unlike the AUTO* tools it does not
		// regenerate the build system — it produces .info manuals that some
		// recipes then INSTALL (glibc's manual/subdir_install installs libc.info).
		// Forcing MAKEINFO=true made that step emit nothing, so the subsequent
		// `install libc.info*` failed. The real makeinfo ships in the base
		// toolchain (gnu.org/texinfo), so let it run.
		"MAKEFLAGS=ACLOCAL=true AUTOMAKE=true AUTOCONF=true AUTOHEADER=true AUTOPOINT=true",
		// Builds run as root in the CI container; autotools' "you should not run
		// configure as root" check would abort otherwise. Bypassing it is the
		// standard build-sandbox posture (Homebrew/distros do the same).
		"FORCE_UNSAFE_CONFIGURE=1",
	}
	// LD_LIBRARY_PATH is passed through when the caller set one. Normally it is
	// absent and must stay so — a host's library path is exactly the kind of
	// leakage the sanitised env exists to stop. But in a FROM-scratch builder
	// there is no system libc at all: every tool, `mkdir` included, finds its
	// libc.so.6 through that variable, and dropping it makes the very first
	// command of a build fail with
	//   mkdir: error while loading shared libraries: libc.so.6: cannot open …
	// The caller opting in (an ENV in the builder image) is a deliberate choice,
	// not ambient host state.
	for _, k := range []string{"LANG", "LOGNAME", "USER", "TERM", "PKGX_PANTRY_DIR", "PKGX_PANTRY_PATH", "GITHUB_TOKEN", "LD_LIBRARY_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// autotools file tiers, oldest → newest, so outputs end up newer than inputs.
var autotoolsTiers = [][]string{
	// Inputs (oldest): configure.ac/.am plus EVERY *.m4 macro — including those
	// under m4/ that ship in a release tarball at a recent mtime. Without ageing
	// them, aclocal.m4 (next tier) can be older than an m4 macro, so make tries
	// to regenerate it with a pinned `aclocal-1.NN` that isn't installed.
	{".ac", ".am", ".m4"},
	// aclocal.m4 is bumped one tier newer than the .m4 macros above (the last
	// matching tier wins), so it never looks stale against them.
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
