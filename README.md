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
| `cmd/bk` | twenty-four subcommands. The pipeline: `build`, `test`, `publish`, `factory`, `builder`, `source`. What a recipe set SAYS: `target`, `versions`, `closure`, `lock`, `catalog`, `tools`, `overrides`, `tohcl`. What it gets WRONG: `depgaps`, `undeclared`, `unresolved`, `lint`, `weather`, `sonames`, `runlog`. Build-time shims a recipe invokes rather than a person: `fixup`, `libtool`, `bkpyvenv`. `bk tools` is worth singling out: it reports which external commands a recipe set invokes, parsed rather than grepped, and `--scope build\|test\|all` separates two surfaces that disagree — a build's is what the IMAGE must hold, a test's is what the package's own acceptance check needs, and **260 tests call a compiler where 6 declare one**. |

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

## Naming a tree, not only a package

A recipe describes one package: `versions`, `distributable`, `build`, `test`,
`provides`, `dependencies`. Until recently the only way to name a **group**
was a flat text file beside the format — `seed/order.txt`, 106 lines, and
`builder/toolchain.txt`, 95 — which could not be versioned, published,
installed or depended on.

A recipe carrying `members` is a **set**:

```hcl
# go-pkgx/base-toolchain
members = {
  # clang, lld, compiler-rt — the compiler itself
  "llvm.org" = "*"

  # libc, crt objects and the dynamic loader
  "gnu.org/glibc" = "*"
}
```

`bk closure go-pkgx/base-toolchain` resolves it. So does `bk factory
--recipes go-pkgx/base-toolchain`, and so would anything else that takes a
project name — a set is expanded **at the root**, before the dependency walk
looks at it, so nothing downstream learns the word.

### Why a recipe and not a new kind of file

The three systems that solved this agree on the shape:

| | |
| --- | --- |
| **Nix** | a tree *is* a derivation. `buildEnv` takes a list of packages and its output is a tree of symlinks. No new kind of object. |
| **Guix** | a manifest names packages; a **profile** is the tree it makes. |
| **Spack** | `spack.yaml` is the abstract set, `spack.lock` the concrete one — the same root specs *"may concretize differently"*. |

Nix's answer is the one taken. A set is an ordinary recipe, so it is signed,
attested, published and installed by everything that already does those
things for a package — rather than a second kind of artefact to keep in step.

### A set is UNORDERED, and that is the point

All three of those manifests are. The order comes out of resolution, not out
of the file, and `bk closure` produces it from the dependency graph.

Which is why **`seed/order.txt` is not a set and must not become one**. That
file is a *build order*: inside a dependency cycle the topological order is a
guess, and `order.txt` is the hand-tuned guess that works — `bk closure
--check-order` exists to judge it. `builder/toolchain.txt` is a different
thing, an *install* list, where order carries nothing; that is the one that
moved.

### `bk lock` — the concrete half

Guix's manual puts it plainly: *"to reproduce a profile bit-for-bit,
manifests alone might not be enough"* — the same names resolve differently
against a different package set, so a manifest needs the channel revisions
beside it. Spack names the two halves: an environment from `spack.yaml` has
*"the same root specs... may concretize differently"*, one from `spack.lock`
has *"the same concrete specs"*.

`members` is the abstract half. `bk lock` is the concrete one:

```
$ bk lock --platform linux/x86-64 -o base-toolchain.lock.hcl go-pkgx/base-toolchain
set: go-pkgx/base-toolchain names 25 member(s)
lock: 41 project(s) pinned → base-toolchain.lock.hcl
```

```hcl
lockfile_version = 1
bk               = "v0.7.0"
platform         = "linux/x86-64"
generated        = "2026-10-05T09:10:39Z"
pantry           = "fd646990024e22f5752578cfdcfe2518b20c0797"
overlay          = "f8f6dcfd7b17410a6de780bdd58c81fbbb38d1b3"
roots            = ["go-pkgx/base-toolchain"]

locked = {
  "curl.se" = { version = "8.17.0", spec = "sha256:5284d597c180…" }
  "gnu.org/bash" = { version = "5.3", spec = "sha256:…" }
  …
}
```

**And it matters more here than in Guix.** A Guix manifest resolves
deterministically once the channel revision is pinned, because the version is
*in* the checkout. A pkgx recipe's `versions:` asks GitHub for tags **at
resolve time**, so pinning the pantry commit is not enough: the same pantry
resolves `gnu.org/binutils` to whatever the newest tag is the day you ask. The
resolved version has to be written down or it is not pinned at all.

The platform is in the header because it changes the answer: the same set
locks to **41** projects on `linux/x86-64` and **40** on `darwin/aarch64`,
`github.com/besser82/libxcrypt` being the difference. A lock that did not say
which platform it was taken on would be read as the other one's.

Two deliberate shapes:

- **Sorted by project, not in build order.** A lock is read as a diff, and a
  topological order makes every line move when one dependency does.
- **An unresolved project exits non-zero** and is named on stderr. A lock with
  a hole in it that reads as a success is a lock somebody commits.

### `-runnable` — build locks and run locks are different files

`bk lock` resolves each version from the recipe's `versions:` block, so it
pins **what the recipes can build**. That is exactly right for
`bk factory --lock`, which is about to build them, and wrong for
`pkgx --lock`, which can only install what the factory has already
**published**. The two run ahead of each other by construction.

Measured 2026-10-09, by generating a lock and running it in a `FROM scratch`
container rather than by reading the code:

```
$ bk lock -platform linux/aarch64 -o curl.lock.hcl curl.se
lock: 1 of 5 pin(s) NOT published for linux/aarch64 — `bk factory --lock` can
      build this file, `pkgx --lock` cannot run it:
    curl.se/ca-certs             2026.09.25     (published here: 2026.8.13)
    `bk lock -runnable` pins what is published instead.
lock: 5 project(s) pinned → curl.lock.hcl
```

Before this, that lock was written in silence and `pkgx --lock` refused it
with a message about a version nobody had chosen.

`-runnable` pins the newest **published** version that satisfies every demand
the closure places on each project — `PickVersionForAll`, the same function
the resolver itself asks, so the lock answers the question pkgx will ask of
it. A per-project "newest published" is **not** a closure: the first version
of this flag asked for `*`, pinned openssl 4.0.2 where `curl.se` demands
`^3`, and produced a file that could not resolve under a line saying it
could.

The mode is written into the file as `pinned = "recipes" | "published"`,
because `bk lock --check` re-resolves — and re-resolving by the other
question reports every pin as having moved.

The verdict is printed **every time**, including when everything is fine:
the defect being fixed was a silence, and a message that appears only on
failure leaves the clean case looking exactly like a `bk` that does not
check at all. It also refuses to endorse a lock with a **hole** in it: a
project that could not be pinned at all leaves the remaining pins all
installable and the file still not a closure.

### `spec` — because a version is not enough either

Spack's packaging guide says what goes into a spec hash: *"`build`, `link`,
and `run` dependencies all affect the hash of Spack packages (along with
`sha256` sums of patches and archives used to build the package, and a
**canonical hash of the `package.py` recipes**)"*.

The last clause is the one worth copying. Pinning a version catches an
upstream that moved; hashing the **recipe** catches the other half — a build
script edited in the pantry resolves to the same version and produces a
different package. A lock with versions alone calls those two builds the same,
which is the thing a lock exists not to do.

`spec` is a Merkle hash over the platform, the project, the resolved version,
the **parsed** recipe (every half of it the closure read) and the spec hashes
of the dependencies. It is taken over the parsed value, not the file, so
reformatting a recipe or rewriting its comments does not move it and changing
what it says does — Spack's "canonical", by a different route.

Measured: of **2091 recipes** in the pantry and the overlay, **0** fail to
serialise. The property that matters is tested both ways round — a change to a
leaf moves the leaf and everything above it; a change at the top moves the top
alone — and that test was checked against a deliberately broken hash, which
fails it, so it is not passing for free.

### `bk lock --check` — a lock nobody reads is a wish too

Cargo's `--locked` and npm's `ci` exist because a lock nobody verifies is a
lock nobody can rely on: both refuse to run when the lock and a fresh
resolution disagree.

```
$ bk lock --check base-toolchain.lock.hcl
lock: linux/x86-64 · 2026-10-05T09:10:39Z · 41 pinned, 2 hour(s) old · pinned from the recipes
lock: pantry revision differs: fd64699… → 9ab12cd…
  gnu.org/binutils                   2.47 → 2.48
  zlib.net                           spec 6a2d499db9ab → spec 90fae00e667c — same version, so something it is built FROM changed
lock: 2 of 41 moved
```

Exit **0** when nothing moved, **1** when something did, **2** when the check
could not be made at all.

**The second line of that diff is the one a version-only lock cannot
produce.** Measured on the real `zlib.net` recipe: a comment added to its
build block moves nothing, and one flag added to its `./configure` line moves
the spec hash while the version stays `1.3.2`.

It takes its roots, its platform, the revisions it expects **and how it was
pinned** from the file. Naming any of them again on the command line would let
the two disagree, and the check would then be of a different question from the
one the file answers — so it refuses a project name, and `-runnable`, beside
`--check`.

A **revision that moved is said and is not fatal**: a pantry can move without
moving any answer, and calling that drift would make the check cry wolf until
nobody ran it. An **unresolved project is kept apart from drift** for the
mirror-image reason — a registry outage must not read as a pantry that
changed.

### The header is data, not a comment

The first version wrote the platform, the roots and the revisions as `#`
lines. Right facts, wrong place: a comment cannot be read back. They are
attributes now, with `lockfile_version` beside them — Spack's lockfile carries
a `lockfile-version` and records which spack wrote it, and the compatibility
rule that comes with it (new readers read old locks; old readers refuse new
ones) is worth copying before it is needed rather than after.

### What is still missing, named rather than left out

The spec hash covers the **inputs**, as Nix's derivation hash and Spack's spec
hash do. It does not cover the **output bytes** of the built bottle. `bottle`
can *verify* a digest but has no exported way to be *asked* for one, and a lock
taken before anything is built has no output to name. The header of every lock
says so, rather than letting a reader assume a guarantee that is not there.

## Where the source came from, and whether it changed

`bk` extracts tarballs from **233 distinct upstream hosts**, and in the
sovereign lane the build that consumes them runs as root inside a chroot. What
checks that?

Almost nothing, before this. `bk` does verify a source checksum when a recipe
declares one — but measured over a fresh `pkgxdev/pantry`, exactly **one recipe
of 904** declares one, and that one (`openssl.org`) points at a `.sha256`
served by the *same host* as the tarball: it detects corruption, not
substitution. The same recipe declares `sig: ${{url}}.asc`, and nothing in bk
reads that field at all.

So `--source-mirror` grew a second job. It already kept every archive a build
downloads, addressed by its sha256; now it also records **which digest each URL
served**, and refuses bytes that disagree:

```
source pin: 9f86d0…  matches what https://ftp.gnu.org/…/sed-4.10.tar.xz served before
fetch: this URL has served different bytes before: https://…/x-1.2.tar.gz
  now serves 2c26b4…, and served 9f86d0… before
```

Trust on first use. It does not make an upstream trustworthy — it makes a
**change** visible, which is the part nobody had.

A version bump is not a change here: the version is in the URL, so a new
release asks a question that has never been asked and is recorded rather than
refused. What fires is the *same* URL serving *different* bytes — a re-cut
release, a compromised mirror, a hijacked domain.

The uncomfortable third case is a store that cannot be **asked**. That is not
an absent pin, and treating the two alike is how trust-on-first-use quietly
becomes trust-every-time. They are distinguished, said out loud, and
`--source-pin-strict` decides which one is fatal — warn and continue by
default, because an unreachable registry is not evidence of tampering and a
build must not die because ghcr hiccuped.

Still open (go-pkgx/bk#282): nothing reads `sig:`. Verifying a detached
signature needs a key policy — *whose* key — and that is a decision, not a
patch.

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
