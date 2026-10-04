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
**1585 projects, 42 746 signed (project, os, arch, version) bottles** as of
2026-10-04 17:00 UTC+2 — linux/aarch64 15 136, linux/x86-64 13 255,
darwin/x86-64 7 113, darwin/aarch64 6 649, windows/x86-64 554, and
**linux/s390x 39**, an architecture being brought up now.

It is a moving target, and more literally than that phrasing suggests: the
same `bk builder --dry-run --platform linux/s390x` reported 25 of 25 toolchain
roots unresolvable at 11:45 and 21 of 25 at 17:03 the same day, because
another session was publishing while this was measured. A registry count is
dated to the HOUR. Re-measure it rather than trusting this line: `go run
./catalog` in
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
| `recipefile` | reads a recipe and applies the project's **logical** override on the way in — nothing on disk is touched, which is what lets the same pantry checkout be read twice |
| `logical` | the override algebra: `set`, substitute, `remove`, `append`, `prepend`, each reporting **applied / already true / premise gone**. A unified diff has two outcomes; the third is what distinguishes a defect from a job upstream has since done |
| `httpretry` | which failed HTTP attempts are worth repeating. Measured on one 23-failure batch, **eleven** were transient HTTP and nothing else — a factory that gives a flaky host exactly one chance reports its own bad luck as a broken recipe |
| `useragent` | the one User-Agent every HTTP path in bk sends. It exists because they differed: `versions` set one and `fetch` did not, so bk could LIST a project's versions and then get 403 downloading the tarball it had just found |
| `versions` | resolves a project's upstream version from the recipe's `versions:` spec — deliberately distinct from what pkgx's dist advertises, which normalises versions the recipe's own source URL does not have |
| `cmd/bk` | twenty-one subcommands. The pipeline: `build`, `test`, `publish`, `factory`, `builder`, `source`. What a recipe set SAYS: `target`, `versions`, `closure`, `tools`, `overrides`, `tohcl`. What it gets WRONG: `depgaps`, `undeclared`, `unresolved`, `lint`, `weather`, `sonames`. Build-time shims a recipe invokes rather than a person: `fixup`, `libtool`, `bkpyvenv`. `bk tools` is worth singling out: it reports which external commands a recipe set invokes, parsed rather than grepped, and `--scope build\|test\|all` separates two surfaces that disagree — a build's is what the IMAGE must hold, a test's is what the package's own acceptance check needs, and **260 tests call a compiler where 6 declare one**. |

`bk build` runs the whole pipeline — resolve version → fetch source → parse
recipe → dependency closure → generate + wrap the build script → run it in a
sanitized env → fix-up → package a bottle. `bk factory` drives that over a whole
list of projects: it expands them to their topologically-ordered runtime-dependency
closure, skips any `(project, version, platform)` already published, applies the
overrides, and publishes each bottle signed with an SBOM and provenance. After
each publish it runs that package's own `test:` block and RECORDS the outcome in
`tests.txt` — four states, one line each — without ever changing the chunk's
result: measured over 120 packages, only 2 of 12 test failures were a package
that does not work, so a gate would stop a run over a missing host config file.
`--test=false` turns it off, `--test-timeout` bounds one package's test, and
`--test-only` builds nothing and tests what the registry already holds — the
factory tests what it publishes once, and that is how a bottle which stopped
working gets noticed afterwards.

`bk test` runs a recipe's own `test:` block against the INSTALLED package, in an
emptied sandbox holding that package, its `test.dependencies` and the recipe's
own files (recipes name fixtures by bare relative name: `cc test.c -lz`) — and a
compiler when the test's own script calls one, which 260 of them do. What it does
not get is the build's flags, its dependencies or its source tree, because the
question is whether what we published works and not whether the build did. It answers in four states, not
two: `0` passed, `1` ran and failed, `3` the recipe declares no test, and `4`
the test never ran (an unresolvable version, an unreachable version source, a
sandbox that could not be made). A 503 from a version source is not a broken
package, and a sweep that cannot tell the two apart files bugs for an outage.

## Building bottles that owe nothing to the build container

`--libc=pkgx` retargets the compiler at the pkgx `gnu.org/glibc` bottle — its crt
objects, its libc, its dynamic linker — instead of the build container's, so the
output runs `FROM scratch`. `--glibc <version>` pins that sysroot to an exact
glibc line, and `bk builder` stages the sovereign rootfs itself: static Go
binaries plus toolchain bottles from the signed registry, nothing else.

[`docs/from-scratch-toolchain.md`](docs/from-scratch-toolchain.md) is the design
note behind it, kept because its hazard list is still the one that bites.

A scratch tree has nothing underneath it, so `bk builder` answers three
questions a build would otherwise answer hours later and attributed to the
wrong thing:

- `--dry-run` resolves the toolchain for `--platform` and stages nothing, so
  "does this architecture have the bottles" costs a round trip rather than a
  runner. On a refusal it names **every** blocked root, not the first — on an
  architecture being brought up, one gap per build is one build per gap — and
  it tells a CONFLICT between roots apart from a missing bottle.
- after staging, it reports every `NEEDED` soname the tree does **not**
  contain, naming who asks. A binary that wants one cannot start, and it says
  so only when something runs it. The s390x second generation met
  `libselinux.so.1` twice, a full build apart, before this existed.
- it says when a prefix is **already** on disk and will not be re-installed.
  `bottle.InstallFor` treats the existence of `<project>/v<ver>` as "already
  present", so a tree left half-written by a run that died — and inherited by
  the next job on a self-hosted runner — is indistinguishable from a complete
  one. Three runs reported "42 packages" and installed none.

None of the three refuses anything: the build that follows is the verdict.

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

## Where it is proven to work

A build tool whose whole premise is *Target ≠ Host* has to be right about
widths and byte order, because that is exactly what it is manipulating: ELF
`DT_RUNPATH` strings rewritten in place, `.dynstr` offsets, tar and xz streams,
OCI manifests. None of that is checked by a compiler. So CI builds eight
targets — linux on amd64, arm64, riscv64, ppc64le, s390x and loong64, plus
darwin/arm64 and windows/amd64, all `CGO_ENABLED=0` — and then **runs the
suite** on five of them under `qemu-user`:

```
test (arm64, qemu)  test (riscv64, qemu)  test (ppc64le, qemu)
test (s390x, qemu)  test (loong64, qemu)
```

`s390x` is there because it is big-endian and nothing else here is; an ELF
header read through the host's byte order instead of `encoding/binary` is wrong
on that machine and on no other. `-count=1`, so a cached PASS from the host
architecture cannot stand in for a run that never happened. The same job holds
the 100% statement-coverage gate: below it, CI prints the uncovered blocks and
fails.

## License

BSD-3-Clause. Copyright the bk authors.
