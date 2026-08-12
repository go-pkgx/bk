package main

import "strings"

// sonameProject maps a shared-library soname STEM (e.g. "libcrypt" for
// "libcrypt.so.1") to the pantry project that provides it. It closes the
// "undeclared runtime library" gap: a tool's binary may NEED a soname its
// package.yml does not declare. The canonical case is perl, whose binary NEEDs
// libcrypt.so.1 (glibc 2.38+ removed libcrypt; crypt() moved to libxcrypt)
// without declaring github.com/besser82/libxcrypt — so the readelf-driven
// completion pulls it from this map. Mirrors go-pkgx/bottle's own soname map so
// both the runtime installer and this from-scratch image builder agree.
var sonameProject = map[string]string{
	// glibc 2.38+ dropped libcrypt — THE key readelf-driven case.
	"libcrypt": "github.com/besser82/libxcrypt",

	// GCC runtime (C++ / unwinding) — undeclared but linked by many tools.
	"libgcc_s":  "gnu.org/gcc/libstdcxx",
	"libstdc++": "gnu.org/gcc/libstdcxx",

	// Compression.
	"libz":    "zlib.net",
	"libbz2":  "sourceware.org/bzip2",
	"liblzma": "tukaani.org/xz",
	"libzstd": "facebook.com/zstd",
	"liblz4":  "lz4.org",

	// Crypto / TLS / transfer.
	"libssl":     "openssl.org",
	"libcrypto":  "openssl.org",
	"libcurl":    "curl.se",
	"libnghttp2": "nghttp2.org",
	"libpsl":     "rockdaboot.github.io/libpsl",

	// Terminal / line editing.
	"libncurses":  "invisible-island.net/ncurses",
	"libncursesw": "invisible-island.net/ncurses",
	"libtinfo":    "invisible-island.net/ncurses",
	"libtinfow":   "invisible-island.net/ncurses",
	"libreadline": "gnu.org/readline",

	// Common C libraries.
	"libffi":       "sourceware.org/libffi",
	"libpcre2-8":   "pcre.org/v2",
	"libintl":      "gnu.org/gettext",
	"libiconv":     "gnu.org/libiconv",
	"libidn2":      "gnu.org/libidn2",
	"libunistring": "gnu.org/libunistring",
	"libexpat":     "libexpat.github.io",
	"libxml2":      "gnome.org/libxml2",
	"libsqlite3":   "sqlite.org",
	"libgmp":       "gnu.org/gmp",
	"libmpfr":      "gnu.org/mpfr",
}

// sonamePrefixProject maps a soname PREFIX to its provider, for projects that
// ship many differently-named sonames (consulted when the exact-stem map misses).
var sonamePrefixProject = map[string]string{
	"libabsl":     "abseil.io",
	"libprotobuf": "protobuf.dev",
	"libboost":    "boost.org",
}

// projectForSoname resolves a NEEDED soname to its provider project via the
// exact-stem map first, then the prefix map. Returns "" if unknown.
func projectForSoname(soname string) string {
	if p := sonameProject[sonameBase(soname)]; p != "" {
		return p
	}
	for pfx, p := range sonamePrefixProject {
		if strings.HasPrefix(soname, pfx) {
			return p
		}
	}
	return ""
}

// sonameBase reduces a soname like "libz.so.1" to its stem "libz".
func sonameBase(soname string) string {
	if i := strings.Index(soname, ".so"); i >= 0 {
		return soname[:i]
	}
	return soname
}

// isGlibcSoname reports whether a soname is one glibc itself provides (the
// glibc bottle is always laid out, so these are satisfied on disk; the check
// keeps the "unresolved soname" warning quiet for glibc-internal names).
func isGlibcSoname(soname string) bool {
	if strings.HasPrefix(soname, "ld-") {
		return true
	}
	switch soname {
	case "libc.so.6", "libm.so.6", "libdl.so.2", "libpthread.so.0",
		"librt.so.1", "libutil.so.1", "libnsl.so.1", "libresolv.so.2",
		"libanl.so.1", "libmvec.so.1", "libnss_files.so.2", "libnss_dns.so.2":
		return true
	}
	return false
}
