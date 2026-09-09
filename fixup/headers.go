package fixup

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
	} {
		m[h] = true
	}
	return m
}()
