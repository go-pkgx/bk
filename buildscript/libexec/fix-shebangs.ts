#!/bin/sh
# fix-shebangs.ts — relocatable shebang rewriter.
#
# A dependency-free (POSIX sh) port of pkgx brewkit's
# libexec/fix-shebangs.ts. Recipes exec it by filename (the `.ts` is just
# part of the name; no deno is required). For each file argument it rewrites
# a `#!<abs-interpreter> [args]` line into the relocatable form
# `#!/usr/bin/env <interpreter-basename>` so the built artifact does not bake
# in the absolute path of a build-time toolchain prefix.
#
# It faithfully matches the upstream behaviour:
#   * arguments that are not regular files are skipped;
#   * files that do not begin with the bytes `#!` (binaries/ELF, data, empty)
#     are skipped;
#   * an already-relocatable shebang (`/usr/bin/env` or `/bin/sh`) is left
#     untouched;
#   * only the interpreter's basename is kept — any interpreter arguments on
#     the shebang line are dropped, exactly as upstream does;
#   * the rest of the file and the file's permission bits are preserved.

set -eu

for path in "$@"; do
	# skip anything that is not a regular file (missing paths included)
	[ -f "$path" ] || continue

	# read the first two bytes; only `#!` scripts are candidates, so binaries
	# (ELF starts with 0x7f 'E'), data and empty files fall through untouched
	magic=$(dd if="$path" bs=1 count=2 2>/dev/null) || continue
	[ "$magic" = "#!" ] || continue

	# read the shebang line (`|| :` so a shebang-only file without a trailing
	# newline still yields the line rather than tripping `set -e` at EOF)
	IFS= read -r line0 < "$path" || :

	# extract the interpreter: the first non-whitespace token after `#!`
	interp=$(printf '%s' "$line0" | sed -n 's/^#![[:space:]]*\([^[:space:]][^[:space:]]*\).*$/\1/p')

	# the interpreter must be an absolute path (upstream's regex requires a
	# leading `/`); a malformed shebang is a hard error, matching upstream
	case "$interp" in
	/*) : ;;
	*)
		echo "fix-shebangs.ts: cannot parse shebang in $path: $line0" >&2
		exit 1
		;;
	esac

	# already-relocatable shebangs are acceptable as-is
	case "$interp" in
	/usr/bin/env | /bin/sh) continue ;;
	esac

	base=${interp##*/}

	tmp="$path.fix-shebangs.$$"
	{ printf '%s\n' "#!/usr/bin/env $base"; tail -n +2 "$path"; } >"$tmp"
	# rewrite in place (via cat, so mode/ownership survive); ensure writable
	chmod u+w "$path" 2>/dev/null || :
	cat "$tmp" >"$path"
	rm -f "$tmp"
done
