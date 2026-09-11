package fixup

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
		return nil
	})
	return checked, problems
}
