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
| `cmd/bk` | twenty-eight subcommands. The pipeline: `build`, `test`, `publish`, `factory`, `builder`, `source`. What a recipe set SAYS: `target`, `versions`, `closure`, `lock`, `catalog`, `tools`, `overrides`, `tohcl`, `version`, `buildenv`, `vulnid`, `advisories`. What it gets WRONG: `depgaps`, `undeclared`, `unresolved`, `lint`, `weather`, `sonames`, `runlog`. Build-time shims a recipe invokes rather than a person: `fixup`, `libtool`, `bkpyvenv`. `bk tools` is worth singling out: it reports which external commands a recipe set invokes, parsed rather than grepped, and `--scope build\|test\|all` separates two surfaces that disagree — a build's is what the IMAGE must hold, a test's is what the package's own acceptance check needs, and **260 tests call a compiler where 6 declare one**. |

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

### `bk version` — because a lock names a bk you have to be able to find

Every lock records which bk wrote it:

```hcl
bk = "v0.15.0"
```

Until now there was no way to ask a binary whether it was that one.
`bk --version` answered `flag provided but not defined: -version` and
`bk version` answered `unknown command: version`, so the question could only
be settled with `go version -m`, and only if you still had the binary rather
than a `bk` on your `PATH`.

```console
$ bk version
bk v0.15.0
```

Both spellings exist and are asserted to give the same answer, because two
code paths that can drift apart are how a tool starts reporting two versions
of itself.

It prints **exactly** what a lock carries, leading `v` included. A prettier
rendering would make the two disagree, and reading a lock and asking *"do I
have that bk?"* is the whole use of the command.

There is **no `-ldflags` stamp**. The sibling tools inject
`-X main.version=<tag>`, which can be set to anything regardless of what was
compiled — that is how a release workflow re-run published binaries labelled
with a tag they were not built from. This reads `debug.ReadBuildInfo`, so it
cannot disagree with the binary it is in, and the release workflow checks it
against the tag by equality alongside the build info of every binary it ships.

### `bk buildenv` — what a published bottle says it was built with

A rebuild for a toolchain vulnerability needs a **before**. When a Go patch
release fixes a standard-library defect, nothing about the recipe or the
upstream version changes — only the compiler in the build environment does. So
"should this be rebuilt?" is "was it built with the fixed one?", and that
question needs the artefact to have recorded an answer.

Until v0.17.0 it recorded nothing. Measured on the published
`stedolan.github.io/jq`, whose **signed** SLSA statement carried:

```json
"builder": { "id": "https://github.com/go-pkgx/bk" },
"buildDefinition": { "externalParameters": {}, "internalParameters": {} }
```

— the same two constants every bottle in the registry holds. `DepTokens`
resolved the whole build and runtime closure on every build and threw the list
away.

Now the provenance carries it, and this reads it back:

```console
$ bk buildenv --platform linux/aarch64 stedolan.github.io/jq 1.8.2
github.com/kkos/oniguruma 6.9.10
go.dev 1.27.2
…
```

**`--list` sweeps**, and counts the three outcomes separately:

```
8 read · 0 record a build environment · 8 record none · 0 unreadable
```

⛔ Those three are never folded together. "Records nothing" is every bottle
published before v0.17.0; "unreadable" is a registry that told us nothing, and
counting it as an absence would be the same defect as a census that could not
read reporting zero. An empty list is a failure rather than a clean registry,
and a line the reader cannot use is named rather than skipped — a silent skip
makes the totals smaller for a reason nobody can see.

That first sweep is the answer to "can a rebuild counter be derived?": **not
from what is published today.** Nothing in the registry is comparable yet, so
the counter starts at zero for everything and becomes meaningful only after a
rebuild under v0.17.0 or later. Debian's rule — *"the absence of a
debian_revision is equivalent to a debian_revision of 0"* — is not a
convention here, it is the literal state of the registry.

#### `--needs` — which bottles a security release obsoletes

The operator's real question after a Go patch release is not "what was this
built with" but **"which of these need rebuilding"**:

```console
$ bk buildenv --platform linux/aarch64 --needs go.dev@1.27.2 --list recipes.txt
STALE   acme.org/tool 1.2.3: go.dev 1.27.1, want >=1.27.2
n/a     zlib.net 1.3.2: does not use go.dev
ok      other.org 2.0.0: go.dev 1.27.2

3 read · 3 record a build environment · 0 record none · 0 unreadable
of those: 1 need rebuilding for go.dev>=1.27.2 · 1 already have it · 1 never used it
```

⛔ **"Does not use it" is its own column.** A C package has no `go.dev`:
counted as stale it would rebuild the whole registry for a Go release, counted
as ok it would make the figure meaningless. That is the mistake a denylist
makes when it inherits a taxonomy's blind spot.

⛔ **And a rebuild of the compiler counts as the compiler.** `go.dev 1.27.1_1`
is 1.27.1 built again, which does not contain a fix that landed in 1.27.2 — so
it is stale, and the version comparison has to see the rebuild suffix to say
so.

The judgement is reported **beside** the reading, never instead of it. Run
against the registry as it stands today:

```
8 read · 0 record a build environment · 8 record none · 0 unreadable
of those: 0 need rebuilding for go.dev>=1.27.2 · 0 already have it · 0 never used it
and 8 record nothing, so this says nothing about them
```

`0 need rebuilding` would read as an all-clear on its own. The last line is
what stops it being one.

### What is still missing, named rather than left out

The spec hash covers the **inputs**, as Nix's derivation hash and Spack's spec
hash do. It does not cover the **output bytes** of the built bottle. `bottle`
can *verify* a digest but has no exported way to be *asked* for one, and a lock
taken before anything is built has no output to name. The header of every lock
says so, rather than letting a reader assume a guarantee that is not there.

## `bk vulnid` — under what name does a CVE database know this package?

Rebuilding a bottle to fix a vulnerability needs a vulnerability to point at,
and pointing at one needs a name both sides agree on. `pkg:pkgx/openssl.org@4.0.2`
is a name **no database knows**: OSV answers `{}` to a `pkg:generic/…` purl, and
NVD's CPE route answers for anything and answers badly — measured on
2026-10-11, `cpeName=…openssl:3.5.7` returned **27 CVEs, four of them from
2009**, because every openssl criterion in the reply reads `openssl:*` with no
upper bound at all.

A recipe usually already names something better, in its `versions:` block.
`bk vulnid` reads it:

```
$ bk vulnid -dir ~/Library/Caches/pkgx/pantry
1563 recipe(s) read from …/pantry/projects
  declared  0
  derived   1241
  none      322
by database:
  GitHub         1186
  crates.io      114
  GitLab         40
  npm            14
  RubyGems       1
```

Three sources, and they rank. **derived** costs nothing: it is read off the
recipe's `versions:` block and off the project's own name. **declared** is an
entry a person wrote in an identity table, and wins, because it exists only
where the derivation was judged wrong or absent. **none** is the honest answer
for the rest — `bk vulnid -list none` names them, and they cluster on a handful
of upstream hosts (`ftp.gnu.org`, `www.x.org`, `sourceforge`), so the table that
fixes them is short.

### The project's own name is a source too

Measured: `crates.io/bpb`, `crates.io/huniq`, `crates.io/stego` and
`crates.io/albion_terminal_rpg` were reported as having **no identity at all**,
while their own names say both the registry and the package. That is four
projects the derivation could answer for and did not.

The larger half is the **110** `crates.io/*` projects that *do* derive a GitHub
repository from their recipe. For those this adds a second, better key rather
than a first one: OSV keys Rust advisories (RUSTSEC) by `crates.io` with
curated version ranges, where a repository-keyed advisory carries whatever
range its filer wrote. Both keys are emitted — two keys mean two databases can
be asked and neither has to be guessed.

This is **not** the same trick as reading a `url:`. A distributable URL says
where the source came from, which is a host and not a package name; that is the
322-project CPE case, and inventing a package name from it is what produced 27
openssl CVEs. A project called `crates.io/bpb` is different in kind: the
pantry's naming convention put the registry and the package name there, and the
package is what the registry calls it.

The host and the ecosystem are not always the same word — `npmjs.com` is the
host and `npm` is the OSV ecosystem — and getting that backwards produces a
query that matches nothing, silently. A test compares the two maps, because
`CPAN` and `CocoaPods` were in the first version of the host map and are not
OSV ecosystems at all.

No network call, ever. The alternative — one query per project — is 1908
requests against a budget this fleet shares with Renovate, and the question
"can this be asked at all?" is answered by the recipes.

### The identity table

```yaml
schema-version: 1
projects:
  openssl.org:
    cpe: [openssl:openssl]
  oracle.com/berkeley-db:
    cpe: [oracle:oracle_berkeley_db, oracle:berkeley_db]
```

Two rules it refuses to bend:

- **A `cpe` name must carry its vendor.** Yocto's documentation gives the reason
  from the other end: *"'tar' has been matching CVEs for the GNU tar package and
  also the node-tar node.js extension. To avoid this problem, use the vendor
  name as a prefix"*. A bare `openssl` is the 27-CVE answer above.
- **A database key is checked against an allowlist**, which is OSV's own list of
  defined ecosystems copied whole, spelling and case included — plus `cpe`,
  `GitHub` and `GitLab`, which are not ecosystems but repository-keyed
  advisories. A typo (`githb:`) in a denylist world is not a typo, it is a
  project that silently has no identity, indistinguishable from one nobody has
  looked at yet.

Several names per database is deliberate, not a mistake: NVD files Berkeley DB
under two product names, and dropping one loses the advisories filed there.

Note also what is **not** in OSV's ecosystem list: `pkgx`. It has `Homebrew`,
`Alpine`, `Wolfi`, `Chainguard` and `vcpkg`. The long-term answer for this fleet
is to become one of those rather than to keep translating into someone else's.

## `bk advisories` — what the answer was, and what we decided

Identity is half of it. The other half is the finding itself, and the record
that **justifies a rebuild**: one file per project, at
`advisories/<project>/advisories.yaml`.

```yaml
schema-version: 1
project: openssl.org
advisories:
  - id: CVE-2026-0001
    aliases: [GHSA-xxxx-xxxx-xxxx]
    events:
      - timestamp: 2026-10-10T12:00:00Z
        type: detection
      - timestamp: 2026-10-11T09:00:00Z
        type: fixed
        fixed-version: 3.5.8_1
```

### Events, not a state

The shape is Wolfi's, and the reason is that a state cannot be superseded.
Wolfi's measured `curl.advisories.yaml` holds **59 advisories and 71 events** —
so most have one and some have three, which is exactly the history a single
`status:` field throws away. OpenVEX says the same from the other end:
`timestamp` is a required field, and a statement's timestamp is "the time at
which the information expressed in the Statement was known to be true". Two
contradictory undated statements cannot be ordered, so neither can be believed.

So: events, each **required** to carry a timestamp, and the last one in time
decides. They are sorted on read, so a rebase that reorders two appends cannot
change what a record means.

The vocabulary is Wolfi's too — `detection`, `true-positive-determination`,
`false-positive-determination`, `fixed`, `fix-not-planned`,
`pending-upstream-fix` — because each maps onto a VEX status, and a record that
cannot be emitted as VEX is a record only this fleet can read.

### `fixed-version` is where the rebuild counter earns its keep

`fixed-version: 3.5.8_1` says the **rebuild** carries the fix, not plain
`3.5.8`. `Advisory.FixedBy` asks `bottle` through the constraint language the
resolver already uses, so `3.5.8` does **not** satisfy it and `3.5.7_9` does
not either. Wolfi's own `fixed-version: 8.5.0-r0` carries the same counter, in
their spelling.

Which is why a Debian-style `3.5.8-1` is **refused**: that is how Debian and
Alpine spell the same thing, `bottle` splits a rebuild on `_`, and the record
would otherwise claim that plain `3.5.8` carries a fix it does not have.

### The path and the file must agree

`bk advisories` refuses a file whose `project:` disagrees with its directory. A
file copied from a neighbouring project and edited in part would otherwise
attach its findings to the wrong package, and nothing else in the record says
which project it is about.

```
$ bk advisories -dir advisories
2 project(s) read from advisories, 3 advisory(ies)
  detection                    2
  true-positive-determination  0
  false-positive-determination 0
  fixed                        1
  fix-not-planned              0
  pending-upstream-fix         0
```

### OpenVEX out

`vuln.EncodeVEX` writes the records as an OpenVEX v0.2.0 document, which is the
form a scanner already reads. The mapping is the design decision, so it is a
table and not a chain of ifs:

| event | VEX status | |
|---|---|---|
| `detection` | `under_investigation` | a database said our NAME matched |
| `true-positive-determination` | `affected` | + an action statement |
| `false-positive-determination` | `not_affected` | + the justification **label** |
| `fixed` | `fixed` | a version of ours carries it |
| `fix-not-planned` | `affected` | + why nothing is coming |
| `pending-upstream-fix` | `affected` | + what is being waited on |

`detection` becoming `under_investigation` rather than `affected` is the one
worth defending: a detection is a database saying our *name* matched, and the
27-CVE openssl measurement above says what that is worth. Emitting those as
`affected` would publish 23 false alarms with our signature on them.

A `not_affected` statement carries the machine-readable `justification` label
and puts the prose in `impact_statement` beside it, never instead of it —
OpenVEX allows the text form and then says it "breaks VEX automation and
interoperability".

Each statement is dated with **its own event's** timestamp, not the document's,
so a publish does not re-date the whole history to today. The encoding is
deterministic — subjects by project, statements by id — because a document that
differed run to run could not be compared with the one already attached to a
bottle.

### The bottle carries its own justification

```
bk publish --to oci://ghcr.io/go-pkgx/bottles --project openssl.org \
           --version 3.5.8_1 --platform linux/x86-64 \
           --advisories ./advisories  openssl.org-3.5.8_1.tar.xz
```

With `--advisories`, the bottle gets a third referrer beside its CycloneDX SBOM
and its in-toto SLSA provenance:

| referrer | says |
|---|---|
| `application/vnd.cyclonedx+json` | what is inside |
| `application/vnd.in-toto+json` | how it was built, and by which `bk` |
| `application/vnd.openvex+json` | which vulnerabilities were considered, and what was decided |

This is what makes a rebuild **self-justifying**. Until now a bottle rebuilt
for a security fix looked exactly like one rebuilt for any other reason: the
version string said `_1` and nothing said why.

Three details earn their keep:

- The statement is bound to the **tarball digest**, the same anchor the SBOM,
  the provenance and the cosign payload use. Four attestations about one
  artefact, not four descriptions that happen to agree.
- The document is dated with the build timestamp, which honours
  `SOURCE_DATE_EPOCH`, and its `@id` carries no date or counter. So re-publishing
  an unchanged bottle produces a byte-identical document — the same referrer
  digest, an idempotent re-push rather than a second referrer beside the first.
  The `@id` *does* carry the platform, because two platforms' bottles are two
  artefacts.
- **A broken advisory file stops the publish.** A file that exists and cannot be
  parsed must not become a silent omission: the bottle would be pushed, look
  complete and assert nothing — and an absent VEX document reads as "nothing was
  ever found", which is the opposite of what a broken record means. A file that
  is simply *absent* is fine and attaches nothing, because most projects have
  never been triaged.

### What this half is still missing

Nothing yet **fills** these files: there is no query against OSV or NVD, so
every record is written by hand today. That is deliberate — one query per
project is 1908 requests against a budget shared with Renovate, and `bk vulnid`
exists to make the query list short first.

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
