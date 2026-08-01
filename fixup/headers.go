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
	} {
		m[h] = true
	}
	return m
}()
