package fixup

import (
	"bytes"
	"path/filepath"
	"strings"
)

// fixStagingScripts strips the +brewing staging prefix out of the scripts a
// build installs.
//
// A recipe configures with the staging prefix, so anything the build bakes into
// an installed file names a directory that CEASES TO EXIST the moment the tree
// is staged onto its final versioned prefix. Measured in the published
// registry:
//
//	openssl.org/v3.0.19/bin/c_rehash:
//	  my $dir = "…/openssl.org/v3.0.19+brewing/ssl";
//	perl.org/v5.28.0/bin/perlivp:
//	  my $perlpath = '…/perl.org/v5.28.0+brewing/bin/perl';
//
// Neither directory exists anywhere, on any machine, including the one that
// built it. Pointing them at the final prefix does not make the bottle
// relocatable — a consumer whose PKGX_DIR differs is still out of luck, and
// only the tool itself can fix that (bison's yacc, for one, carries gnulib's
// relocatable-sh and works out its own prefix from $0) — but it turns a path
// that is wrong everywhere into one that is right wherever the bottle is
// installed under the same PKGX_DIR, which is the case for the factory's own
// builds and for the sovereign builder.
//
// Only files beginning with `#!` are touched. Rewriting an ELF or Mach-O this
// way would corrupt it: the replacement is shorter, and every offset after it
// would shift. rpath.go and macho.go handle those, in place and at fixed width.
func fixStagingScripts(prefix, buildInstall string, log func(string, ...any)) error {
	if buildInstall == "" || buildInstall == prefix {
		return nil
	}
	for _, part := range []string{"bin", "sbin", "libexec"} {
		if err := walkScripts(filepath.Join(prefix, part), buildInstall, prefix, log); err != nil {
			return err
		}
	}
	return nil
}

// walkScripts recurses a directory rewriting the staging prefix in every file
// that opens with a shebang.
func walkScripts(dir, buildInstall, prefix string, log func(string, ...any)) error {
	if !isDir(dir) {
		return nil
	}
	ents, err := osReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if err := walkScripts(p, buildInstall, prefix, log); err != nil {
				return err
			}
			continue
		}
		b, err := osReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(b, []byte("#!")) || !strings.Contains(string(b), buildInstall) {
			continue
		}
		if err := rewriteFile(p, buildInstall, "", prefix, log); err != nil {
			return err
		}
	}
	return nil
}
