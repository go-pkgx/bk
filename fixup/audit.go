package fixup

import (
	"errors"
	"fmt"
)

// AuditRelocatable runs the Mach-O relocatability guards over an already-laid-out
// prefix WITHOUT modifying a byte, and reports what it found.
//
// `FixUp` runs these same checks while it rewrites a freshly built package, so
// nothing it produces can reach the registry unrunnable. A MIRRORED bottle is
// never unpacked — it is republished verbatim — so the guards have no bytes to
// look at and cannot fire. `git-scm.org 2.55.0` reached the catalogue that way:
// 164 of its 164 Mach-O files reach a sibling package only through
// `/Users/runner/.pkgx`, the build machine's own path, so `git` cannot load
// zlib on any other machine. It surfaced weeks later as some other recipe's
// build failure — `git-remote-https died of signal 6`.
//
// This reports and does not refuse. Whether a mirror is REQUIRED to be
// relocatable is a question about what our signature claims, and it is
// go-pkgx/packages#147's to answer; what a publisher should not have to do is
// find out from a third package's dyld error.
func AuditRelocatable(prefix, pkgxDir string) (checked int, problems []error) {
	opts := Options{PkgxDir: pkgxDir}
	_ = walkExes(prefix, func(p string) error {
		if !isMachO(p) {
			return nil
		}
		checked++
		if err := checkRpathResolvable(p, opts); err != nil {
			problems = append(problems, err)
		}
		if err := checkNoDuplicateRpath(p); err != nil {
			problems = append(problems, err)
		}
		return nil
	})
	return checked, problems
}

// ErrDuplicateRpath means a Mach-O carries the same LC_RPATH twice. ld refuses
// to link against such a library, so it breaks every dependent rather than the
// bottle itself — which is why nothing noticed until a dependent's build failed:
//
//	ld: duplicate LC_RPATH '@loader_path/../../../..' in
//	    .../facebook.com/folly/v2026.09.14.00/lib/libfolly.0.58.0-dev.dylib
//	c++: error: linker command failed with exit code 1
//
// It was this factory that wrote the second one: relativising an absolute rpath
// can produce a string bk had already linked in. That is fixed where it is
// made, but a defect invisible to every check we had is exactly what a guard is
// for — the bottle carrying it was built, signed, published and inspected
// without complaint.
var ErrDuplicateRpath = errors.New("fixup: duplicate LC_RPATH")

// checkNoDuplicateRpath reports a Mach-O that names one rpath more than once.
// The caller has already established that exe parses: isMachO IS "machoInfo
// succeeds", and machoRpaths fails nowhere else — so the read cannot fail here,
// and a branch no test can reach is a line the coverage gate is right to refuse.
func checkNoDuplicateRpath(exe string) error {
	rpaths, _ := machoRpaths(exe)
	seen := map[string]bool{}
	for _, r := range rpaths {
		if seen[r] {
			return fmt.Errorf("%w: %s names %q more than once; ld refuses to link against it", ErrDuplicateRpath, exe, r)
		}
		seen[r] = true
	}
	return nil
}
