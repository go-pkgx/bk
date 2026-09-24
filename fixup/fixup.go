// Package fixup performs the post-build relocatability fix-ups brewkit applies
// before a package is bottled: rewriting hardcoded paths in .pc/.cmake files
// and in installed scripts, removing libtool .la files, consolidating
// lib64→lib, flattening single-dir include trees, and (via rpath.go) fixing
// ELF RUNPATHs.
//
// A Windows target needs NONE of this: a PE has no rpath/RUNPATH (DLLs colocate
// next to the .exe or on PATH), there is no Mach-O or ELF to patch, no lib64
// split, and no shebang rewriting — FixUp returns immediately.
package fixup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Options controls a fix-up run.
type Options struct {
	Prefix       string   // the final install prefix (…/project/vX.Y.Z)
	BuildInstall string   // the +brewing staging prefix that paths were baked with
	Platform     string   // target platform: darwin | linux | windows
	PkgxDir      string   // $PKGX_DIR: the root darwin @rpath references resolve against
	Skips        []string // recipe build.skip entries (fix-machos, fix-patchelf, libtool-cleanup, flatten-includes)
	// DepPaths are the install prefixes of the build's dependency closure,
	// used to compute $ORIGIN-relative RUNPATHs (linux only).
	DepPaths []string
	// Log, if set, receives human-readable progress lines.
	Log func(string)
}

func (o *Options) log(format string, a ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(format, a...))
	}
}

func has(skips []string, s string) bool {
	for _, x := range skips {
		if x == s {
			return true
		}
	}
	return false
}

// FixUp runs the full relocation pipeline for opts.Platform.
func FixUp(opts Options) error {
	if opts.Platform == "windows" {
		opts.log("windows target: skipping POSIX relocation for %s", opts.Prefix)
		return nil
	}
	if err := fixRpaths(opts); err != nil {
		return err
	}
	if err := fixPCFiles(opts.Prefix, opts.BuildInstall, opts.log); err != nil {
		return err
	}
	if err := fixCMakeFiles(opts.Prefix, opts.BuildInstall, opts.log); err != nil {
		return err
	}
	if err := fixStagingScripts(opts.Prefix, opts.BuildInstall, opts.log); err != nil {
		return err
	}
	if !has(opts.Skips, "libtool-cleanup") {
		if err := removeLaFiles(opts.Prefix, opts.log); err != nil {
			return err
		}
	} else {
		opts.log("skipping libtool cleanup for %s", opts.Prefix)
	}
	if opts.Platform == "linux" {
		if err := consolidateLib64(opts.Prefix, opts.log); err != nil {
			return err
		}
	}
	if !has(opts.Skips, "flatten-includes") {
		if err := flattenHeaders(opts.Prefix, opts.log); err != nil {
			return err
		}
	} else {
		opts.log("skipping header flattening for %s", opts.Prefix)
	}
	return auditBuilt(opts)
}

// auditBuilt runs the relocatability guards over the package we just built.
//
// They existed and did not run here. AuditRelocatable is called from exactly
// one place — the factory's MIRROR path — with the comment "a mirror is never
// unpacked, so fixup's relocatability guards never run on it". True, and it
// left the inverse unsaid: on the build path only checkRpathResolvable ran, so
// a stale code signature, a 32-bit magic over a 64-bit cputype, a duplicate
// LC_RPATH and an absolute /opt/homebrew reference were all checked on the
// bottles we COPY and on none of the bottles we MAKE.
//
// What is fatal here is what is never correct and always fatal later:
//
//	stale signature  -> SIGKILL at the first page fault, no output at all
//	magic mismatch   -> the kernel kills it, likewise silently
//	duplicate rpath  -> ld refuses to link any CONSUMER against it
//	absolute ref     -> names a path that exists on the builder and nowhere else
//
// A missing @rpath target is REPORTED instead. Measured over 26519 Mach-O in
// 272 installed closures, 51 packages carry one — and qt.io is among them and
// runs, because a dangling reference in a module nothing loads never faults.
// Refusing would stop a fifth of the catalogue over latent defects; saying so
// at build time puts them in the log where they can be drained.
// No windows guard here: FixUp returns before this on that platform, and a
// branch no test can reach is a line the coverage gate is right to refuse.
func auditBuilt(opts Options) error {
	return walkExes(opts.Prefix, func(p string) error {
		if !isMachO(p) {
			return nil
		}
		for _, check := range []func(string) error{
			checkMachoSignature, checkMachoMagic, checkNoDuplicateRpath,
		} {
			if err := check(p); err != nil {
				return err
			}
		}
		// An absolute reference INTO $PKGX_DIR is this package's deliberate
		// fallback, not a leak: rewriteMacho leaves a name absolute when no
		// rpath reaches the store, because "@rpath would resolve to nothing,
		// which is worse than a path that at least works on one machine".
		// Refusing it here would fail the build where today it produces
		// something usable, and the real remedy — an rpath that reaches — is a
		// recipe change. Reported.
		//
		// Absolute anywhere ELSE is never that fallback. /opt/homebrew and
		// /Users/builder name a machine that is not the user's, and no rpath
		// decision produces them: they are what a configure script found lying
		// around. Refused.
		// underDir already answers false for an empty dir, so no guard here.
		intoTheStore := func(ref string) bool {
			_, ok := underDir(ref, opts.PkgxDir)
			return ok
		}
		if err := checkAbsoluteRefs(p, intoTheStore); err != nil {
			return err
		}
		if err := checkAbsoluteRefs(p, nil); err != nil {
			opts.log("fixup: %v", err) // the tolerated fallback, said out loud
		}
		if err := checkRefExists(p, opts); err != nil {
			opts.log("fixup: %v", err)
		}
		return nil
	})
}

// fixPCFiles rewrites absolute build/install prefixes in pkg-config .pc files
// to ${pcfiledir}-relative form so the bottle relocates.
func fixPCFiles(prefix, buildInstall string, log func(string, ...any)) error {
	for _, part := range []string{"share", "lib"} {
		d := filepath.Join(prefix, part, "pkgconfig")
		if !isDir(d) {
			continue
		}
		ents, err := osReadDir(d)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if e.IsDir() || filepath.Ext(e.Name()) != ".pc" {
				continue
			}
			p := filepath.Join(d, e.Name())
			rel := relTo(prefix, d) // prefix relative to the .pc's dir
			if err := rewriteFile(p, buildInstall, prefix, "${pcfiledir}/"+rel, log); err != nil {
				return err
			}
		}
	}
	return nil
}

// fixCMakeFiles rewrites absolute prefixes in installed .cmake files to
// ${CMAKE_CURRENT_LIST_DIR}-relative form.
func fixCMakeFiles(prefix, buildInstall string, log func(string, ...any)) error {
	cmake := filepath.Join(prefix, "lib", "cmake")
	if !isDir(cmake) {
		return nil
	}
	return walkCMake(cmake, prefix, buildInstall, log)
}

// walkCMake recurses a directory (via the osReadDir seam) rewriting .cmake files.
func walkCMake(dir, prefix, buildInstall string, log func(string, ...any)) error {
	ents, err := osReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if err := walkCMake(p, prefix, buildInstall, log); err != nil {
				return err
			}
			continue
		}
		if filepath.Ext(p) != ".cmake" {
			continue
		}
		rel := relTo(prefix, filepath.Dir(p))
		if err := rewriteFile(p, buildInstall, prefix, "${CMAKE_CURRENT_LIST_DIR}/"+rel, log); err != nil {
			return err
		}
	}
	return nil
}

// rewriteFile replaces both the +brewing (build) prefix and the final prefix
// with repl in one file, writing back only if something changed.
func rewriteFile(path, buildInstall, prefix, repl string, log func(string, ...any)) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	orig := string(b)
	text := orig
	if buildInstall != "" {
		text = strings.ReplaceAll(text, buildInstall, repl)
	}
	// An empty prefix means "only the staging prefix" — ReplaceAll on "" would
	// splice repl between every character of the file.
	if prefix != "" {
		text = strings.ReplaceAll(text, prefix, repl)
	}
	if text != orig {
		log("fixing %s", path)
		info, err := osStat(path)
		if err != nil {
			return err
		}
		restore, err := ensureWritable(path)
		if err != nil {
			return err
		}
		defer restore()
		return osWriteFile(path, []byte(text), info.Mode().Perm())
	}
	return nil
}

// removeLaFiles deletes top-level lib/*.la (hardcoded-path libtool archives).
// Subdirectory .la files may be runtime module descriptors, so are left alone.
func removeLaFiles(prefix string, log func(string, ...any)) error {
	lib := filepath.Join(prefix, "lib")
	if !isDir(lib) {
		return nil
	}
	ents, err := osReadDir(lib)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".la" {
			continue
		}
		p := filepath.Join(lib, e.Name())
		log("removing %s", p)
		if err := osRemove(p); err != nil {
			return err
		}
	}
	return nil
}

// consolidateLib64 moves lib64/* into lib/ and replaces lib64 with a symlink,
// standardising on lib for x86-64 Linux builds that install to lib64.
func consolidateLib64(prefix string, log func(string, ...any)) error {
	lib64 := filepath.Join(prefix, "lib64")
	fi, err := os.Lstat(lib64)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil //nolint (missing or already a symlink)
	}
	lib := filepath.Join(prefix, "lib")
	if err := osMkdirAll(lib, 0o755); err != nil {
		return err
	}
	ents, err := osReadDir(lib64)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := osRename(filepath.Join(lib64, e.Name()), filepath.Join(lib, e.Name())); err != nil {
			return err
		}
	}
	if err := osRemove(lib64); err != nil {
		return err
	}
	log("consolidated lib64 → lib")
	return osSymlink("lib", lib64)
}

// flattenHeaders collapses include/<single-subdir>/*.h up into include/ (with
// include/<subdir> becoming a self-symlink) when include/ holds exactly one
// entry that is a directory — unless doing so would shadow a system header.
func flattenHeaders(prefix string, log func(string, ...any)) error {
	include := filepath.Join(prefix, "include")
	if !isDir(include) {
		return nil
	}
	ents, err := osReadDir(include)
	if err != nil {
		return err
	}
	if len(ents) != 1 || !ents[0].IsDir() {
		return nil
	}
	name := ents[0].Name()
	subdir := filepath.Join(include, name)
	subents, err := osReadDir(subdir)
	if err != nil {
		return err
	}
	var dominated []string
	if !intentionalShadow[strings.ToLower(name)] {
		for _, e := range subents {
			if systemHeaderNames()[strings.ToLower(e.Name())] {
				dominated = append(dominated, e.Name())
			}
		}
	}
	if len(dominated) > 0 {
		log("skipping flatten of %s (would shadow %s)", name, strings.Join(dominated, ", "))
		return nil
	}
	for _, e := range subents {
		if err := osRename(filepath.Join(subdir, e.Name()), filepath.Join(include, e.Name())); err != nil {
			return err
		}
	}
	if err := osRemove(subdir); err != nil {
		return err
	}
	log("flattened headers %s", name)
	return osSymlink(".", subdir)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// relTo returns target relative to base in slash form (for embedding in text
// files), falling back to target unchanged if no relative path exists.
func relTo(target, base string) string {
	r, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(r)
}
