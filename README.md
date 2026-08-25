# bk — a pure-Go brewkit

`bk` is a from-scratch, `CGO_ENABLED=0` reimplementation of [pkgx](https://pkgx.sh)'s
build tool [brewkit](https://github.com/pkgxdev/brewkit) (originally Deno/TypeScript
with Ruby + `patchelf` helpers). It reads a pantry `package.yml`, builds the package
in a pkgx environment, relocates the install tree, and packages a bottle.

The reference brewkit is a *native* build tool — everything pivots on the host it
runs on being the thing it builds for. `bk` is **Target ≠ Host by design**: the
build target (platform/arch/triple) is a first-class value, so cross-compilation —
notably **Windows PE bottles built on a linux/darwin host** — is a supported path
rather than a retrofit.

## Status

In production: `bk factory` is what fills
[`ghcr.io/go-pkgx/packages`](https://github.com/orgs/go-pkgx/packages) —
**1459 projects, 26 398 signed (project, os, arch, version) bottles** across
linux/aarch64, linux/x86-64, darwin/aarch64, darwin/x86-64 and windows/x86-64 as
of 2026-08-24. It is a moving target — re-measure it rather than trusting this
line: `go run ./catalog` in
[go-pkgx/packages](https://github.com/go-pkgx/packages) enumerates the registry
itself, and <https://go-pkgx.github.io/packages> browses it.

The packages, all at 100% statement coverage (`go test ./... -coverprofile` + `go tool cover -func`, enforced in CI):

| package  | what it does |
|----------|--------------|
| `target` | the single source of truth for the build **target** vs the **host** (`BREWKIT_TARGET` / `--platform`), incl. the llvm-mingw Windows cross triples |
| `config` | the build workspace layout, keyed on the target so a cross build never collides with a native one |
| `schema` | an authoritative **JSON Schema** for `package.yml` — there is no official one; this is derived from libpkgx's parser and validated against the entire pkgxdev/pantry corpus (0 rejections over 1890 recipes) |
| `pantry` | parses + schema-validates a `package.yml` into a shared `Recipe` |
| `pantry/hcl` | an **HCL2 front-end** — a `package.hcl` decodes into the *same* `Recipe` via the same schema, so a recipe written in HCL2 yields an identical build |
| `fixup`  | post-build relocatability: `.pc`/`.cmake` path rewriting, libtool `.la` cleanup, `lib64→lib`, single-dir header flattening, and a **pure-Go ELF RUNPATH rewriter** (`debug/elf`, no `patchelf`). A Windows target skips all POSIX relocation. |
| `moustache` | pkgx's `{{token}}` template substitution (version/deps/prefix/hw) |
| `buildscript` | the build/test script generator (`if:` guards vs the target, platform-reduced env, fixtures) + the porcelain `Wrap` (full runnable script) |
| `fetch` | source download + extract (tar.gz/xz/bz2/zip, git), zip-slip-safe |
| `bottlepkg` | package an install tree into a pkgx bottle + dist layout |
| `build` | the pipeline orchestrator (`Runner`): dep-closure, base toolchain, sanitized env, autotools maintainer-mode defeat |
| `overrides` | applies the factory's local recipe-override patches to a pantry checkout in pure Go — a `git diff` parsed and applied without shelling out to `git apply`, and idempotent (it resets the files it touches first) |
| `versions` | resolves a project's upstream version from the recipe's `versions:` spec — deliberately distinct from what pkgx's dist advertises, which normalises versions the recipe's own source URL does not have |
| `cmd/bk` | `target`, `fixup`, `versions`, `build`, `publish`, `closure`, `depgaps`, `builder`, `factory` |

`bk build` runs the whole pipeline — resolve version → fetch source → parse
recipe → dependency closure → generate + wrap the build script → run it in a
sanitized env → fix-up → package a bottle. `bk factory` drives that over a whole
list of projects: it expands them to their topologically-ordered runtime-dependency
closure, skips any `(project, version, platform)` already published, applies the
overrides, and publishes each bottle signed with an SBOM and provenance.

## Building bottles that owe nothing to the build container

`--libc=pkgx` retargets the compiler at the pkgx `gnu.org/glibc` bottle — its crt
objects, its libc, its dynamic linker — instead of the build container's, so the
output runs `FROM scratch`. `--glibc <version>` pins that sysroot to an exact
glibc line, and `bk builder` stages the sovereign rootfs itself: static Go
binaries plus toolchain bottles from the signed registry, nothing else.

[`docs/from-scratch-toolchain.md`](docs/from-scratch-toolchain.md) is the design
note behind it, kept because its hazard list is still the one that bites.

### Recipes in YAML or HCL2

The `package.yml` schema (`schema/package.schema.json`) doubles as editor
autocompletion — add to a recipe:

```yaml
# yaml-language-server: $schema=https://go-pkgx.github.io/bk/package.schema.json
```

The same recipe in HCL2 (`package.hcl`) decodes to the identical `Recipe`:

```hcl
distributable {
  url              = "https://curl.se/download/curl-{{version}}.tar.bz2"
  strip-components = 1
}
dependencies = { "openssl.org" = "^1.1", "zlib.net" = "^1.2.11" }
build {
  script = ["./configure $ARGS", "make --jobs {{ hw.concurrency }} install"]
  env    = { ARGS = ["--prefix={{prefix}}", "--with-openssl"] }
}
provides = ["bin/curl", "bin/curl-config"]
```

## Why

Completes the pure-Go [`go-pkgx`](https://github.com/go-pkgx) family (bottle / pkgm /
pkgx / mirror) with the last Deno piece — a single static binary that builds pantry
recipes with no Deno/Ruby/patchelf runtime, and cross-builds cleanly.

## The ELF RUNPATH rewriter

`fixup.SetRunpath` overwrites an ELF's `DT_RUNPATH`/`DT_RPATH` string **in place**
(pure Go): it locates the string offset in `.dynstr` and rewrites the bytes,
zero-padding slack. It cannot grow `.dynstr`, so the build links binaries with a
long `-Wl,-rpath` placeholder that the final `$ORIGIN`-relative value fits inside —
the same budget model `patchelf --set-rpath` sidesteps by rewriting the file.

## License

BSD-3-Clause. Copyright the bk authors.
