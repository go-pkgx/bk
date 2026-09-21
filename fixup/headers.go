package fixup

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// systemHeaders are header basenames (lower-cased) that must not be flattened
// into include/ as they would shadow system/libc headers. Compared
// case-insensitively for macOS HFS+/APFS.
var systemHeaders = func() map[string]bool {
	m := map[string]bool{}
	for _, h := range []string{
		// C standard
		"assert.h", "complex.h", "ctype.h", "errno.h", "fenv.h", "float.h",
		"inttypes.h", "iso646.h", "limits.h", "locale.h", "math.h", "setjmp.h",
		"signal.h", "stdalign.h", "stdarg.h", "stdatomic.h", "stdbool.h",
		"stddef.h", "stdint.h", "stdio.h", "stdlib.h", "stdnoreturn.h",
		"string.h", "tgmath.h", "threads.h", "time.h", "uchar.h", "wchar.h",
		"wctype.h",
		// POSIX
		"dirent.h", "fcntl.h", "glob.h", "grp.h", "netdb.h", "poll.h",
		"pthread.h", "pwd.h", "regex.h", "sched.h", "search.h", "semaphore.h",
		"spawn.h", "strings.h", "syslog.h", "termios.h", "unistd.h", "utime.h",
		"wordexp.h",
		// common C++ / platform headers that cause trouble
		"memory.h", "version.h", "module.h",
		// Extension headers: neither C-standard nor POSIX, which is why the two
		// lists above missed them, and shadowing one is just as fatal.
		//
		// xlocale.h cost a day. libX11 ships Xlocale.h, this flatten put it at
		// the include root, CPATH puts that root ahead of the SDK, and macOS
		// filesystems are case-insensitive — so libc++'s `#include <xlocale.h>`
		// opened X11's header, which declares no locale_t and no LC_*_MASK.
		// Every darwin C++ translation unit reaching <locale> then failed, and
		// <locale> is reached by <functional> and <vector>: llvm.org stopped
		// building on darwin entirely, with an error inside Apple's own headers
		// that pointed nowhere near here.
		//
		// The comparison below has always been case-insensitive. Only the list
		// was short.
		"xlocale.h",
		// err.h is the SECOND one to cost a day, and it arrived the same way.
		// openssl.org ships 142 headers, this flatten put them all at the
		// include root, and openssl's own err.h then answered every consumer's
		// `#include <err.h>`. developers.yubico.com/libfido2's tools call
		// errx(3):
		//   tools/cred_make.c:37:3: error: call to undeclared function 'errx'
		// and two attempts to fix it — -Wno-implicit-function-declaration, then
		// -include err.h — were both defeated, the second because -include
		// resolves against the -I path too, so it opened openssl's header as
		// well. Reproduced locally by putting a decoy err.h on -I.
		//
		// These are BSD/GNU extension headers that macOS and glibc both ship
		// and that neither list above can reach. The list is a FLOOR now: see
		// systemHeaderNames, which adds whatever the machine actually has.
		"err.h", "sysexits.h", "fts.h", "libgen.h", "paths.h", "getopt.h",
		"alloca.h", "ar.h", "cpio.h", "tar.h", "ftw.h", "fnmatch.h",
		"langinfo.h", "monetary.h", "nl_types.h", "iconv.h", "dlfcn.h",
		"stdio_ext.h", "byteswap.h", "endian.h", "features.h", "sysexits.h",
	} {
		m[h] = true
	}
	return m
}()

// systemHeaderNames is systemHeaders plus every header the MACHINE actually
// carries, read once from the system include directories.
//
// A curated list inherits the blind spot of the taxonomy it was drawn from.
// This one was C-standard plus POSIX, so it missed xlocale.h (an X11 clash that
// stopped llvm.org building on darwin) and then err.h (an openssl clash that
// stopped libfido2 building at all) — both extension headers, neither in either
// standard, each found only after it had broken something.
//
// The filesystem has no taxonomy. Where it can be read it is the better answer,
// and the list above remains the floor for where it cannot — a from-scratch
// rootfs, or a cross-build whose target headers are not on this machine.
func systemHeaderNames() map[string]bool {
	systemHeadersOnce.Do(func() {
		m := map[string]bool{}
		for k := range systemHeaders {
			m[k] = true
		}
		for _, dir := range systemIncludeDirs() {
			ents, err := osReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if n := e.Name(); strings.HasSuffix(n, ".h") {
					m[strings.ToLower(n)] = true
				}
			}
		}
		systemHeadersCache = m
	})
	return systemHeadersCache
}

var (
	systemHeadersOnce  sync.Once
	systemHeadersCache map[string]bool
)

// systemIncludeDirs are the places a system header can be read from, most
// portable first. SDKROOT is what xcrun exports and what every cc on a mac
// respects; /usr/include is glibc's and, where the Command Line Tools are
// installed, macOS's too.
func systemIncludeDirs() []string {
	dirs := []string{"/usr/include"}
	if r := os.Getenv("SDKROOT"); r != "" {
		dirs = append(dirs, filepath.Join(r, "usr", "include"))
	}
	return dirs
}
