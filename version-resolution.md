# How five package managers choose a version, and where we sit

Written after the s390x seed spent three dispatches building versions nothing
could use. The question underneath was simple and I had not asked it: **when a
project appears in a closure only because something depends on it, which of its
versions do we build?**

This note records what the other systems answer, because the answer decides how
much machinery a factory needs — and we had been carrying the constraint
surface of one design with the resolver of another.

## Nix and Guix: there is no question

Nix has **no dependency resolution at all**. It has no concept of "versions",
"constraints" or package name resolution; a nixpkgs tree contains one
expression per package, and what you get is whatever that expression says.
Guix is the same model. The whole repository is meant to be installable
together, so the resolver's job is **topological sorting rather than constraint
satisfaction** ([Dependency solving in Nix][nixdep], [LWN on Nix and
Guix][lwn]).

Variation is expressed by **rewriting the graph**, not by solving it. Guix's
`--with-input=guile=guile-next` substitutes one node for another across the
whole graph; `--with-version`, `--with-patch` and friends do the same for other
axes ([Package Transformation Options][guixtrans]). No SAT solver is involved
because no constraint is ever in tension: a curated tree has one answer by
construction.

The cost is that the tree is the unit of curation. You cannot ask nixpkgs for
"openssl 3 for this consumer and openssl 1 for that one" without building a
second tree.

## Spack: the question is NP-complete, so it uses a solver

Spack sits at the other end. Its concretizer compiles package constraints into
**Answer Set Programming** facts and hands them to **clingo**, which grounds
them into propositional logic and solves with SAT/SMT techniques — DPLL, CDCL
([Using ASP for HPC Dependency Solving][asp], [spack.solver][solverdoc]).

Two things in that design matter here.

First, **"use the newest satisfying version of every package" is an
optimization objective**, not an algorithm. It sits beside "maximize reusable
specs" in a global solve, so Spack can give up newness on one package to make
another satisfiable.

Second, Spack says plainly that dependency resolution *considering only
compatible versions* is already NP-complete, and that it must also decide build
options, operating systems and microarchitectures.

## Homebrew: one version, and a counter for rebuilds

Homebrew carries a single formula per package. When a dependent fails to build
against a new dependency, the dependency gets a **revision** — a forced
recompile — rather than the dependent getting an older version
([Formula Cookbook][brewcookbook]). Recent work adds `compatibility_version`,
recorded in the installed tab so an upgrade can skip rebuilding dependents that
are still known to be compatible ([brew#20804][brew20804]).

So Homebrew answers the question by refusing to have it: there is one version,
and the machinery is about *when to rebuild*, not *what to select*.

## pkgx: a constraint per edge, resolved greedily

pkgx reads `package.yml` and `versions.txt` and walks the runtime closure
breadth-first, **picking the highest `versions.txt` entry satisfying each
constraint** — `*`, `^`, `~`, `>=`, `=` ([Contributing Packages][pkgxpantry]).

That is per-edge greedy resolution: no backtracking, no global objective. It is
much less than Spack and much more than Nix.

## Where we sat, and why it broke

Our **client** does what pkgx does — `ResolveClosureFor` picks the newest
version satisfying the constraint on each edge.

Our **factory** did not. `versionsFor` resolved every closure-only dependency
against `"*"`:

```go
if !requested {
    v, _, err := factoryResolve(rec.Versions, "*")   // the newest, always
```

So the two halves of the same system disagreed about which version mattered,
and the client is the half users experience. The factory built `mpdecimal
4.0.1` because it is newest; `python.org` asks for `2`; and the build that
consumed it said so:

```
python.org 3.14.7: resolve deps: no version of bytereef.org/mpdecimal
satisfies "2" (available: 1)
```

Measured over the s390x seed closure with `bk closure --build --constraints`:
**49 projects carry a constraint, and all 49 were being resolved to their
newest.**

That is not a Nix-shaped problem — we are not a curated single-version tree,
our registry holds many versions per project. It is a pkgx-shaped problem that
the factory was not solving at all.

## What we do now, and what it is not

`closureOf` records what each dependent asked for, and a closure-only
dependency resolves to the **newest version satisfying every one of them**
(bk#213). The candidate list is filtered by the constraints; the constraint
*strings* are not intersected, because an interval solver has to invent a
version inside the interval and nothing guarantees one was released.

**This is greedy and local, like pkgx and unlike Spack.** It does not
backtrack. If choosing A's newest admissible version painted B into a corner, a
concretizer would back up and this will not; it will report that B has no
version satisfying its dependents and stop. For a curated pantry of a few
thousand recipes that has been enough, and the failure mode is a refusal with
the constraints named rather than a wrong build.

Where dependents genuinely disagree there is no version to choose, and
`bk closure --pins` says so instead of picking:

```
# NO single version: perl.org  ~5.42 (gnu.org/binutils, openssl.org)  vs  ~5.44 (gnu.org/gettext, …)
# NO single version: llvm.org  20 (doxygen.nl)  vs  21 (rust-lang.org)  vs  <19 (perl.org)
```

Spack would resolve those by building both and letting the solver place each
consumer on the right one. So can we — they are separate builds — but nothing
plans that for us today.

## One idea worth stealing

Spack's `patch()` directive takes a **sha256** and a **`when=`** version scope:
`patch("foo.patch", when="@1.0.0:")` ([Packaging Guide][spackpkg]).

Our `overrides/*.patch` have neither. A patch applies to whatever the pantry
currently says, and when upstream edits the lines its context hangs on it
silently stops applying — 3 of 231 had, on 2026-09-24. `bk overrides` (#215)
now refuses a run where any patch no longer applies, which catches the drift;
binding a patch to the version it was written against would prevent it.

[nixdep]: http://www.chriswarbo.net/projects/nixos/nix_dependencies.html
[lwn]: https://lwn.net/Articles/962788/
[guixtrans]: https://guix.gnu.org/manual/1.5.0/en/html_node/Package-Transformation-Options.html
[asp]: https://arxiv.org/pdf/2210.08404
[solverdoc]: https://spack.readthedocs.io/en/latest/spack.solver.html
[spackpkg]: https://spack.readthedocs.io/en/latest/packaging_guide_creation.html
[brewcookbook]: https://docs.brew.sh/Formula-Cookbook
[brew20804]: https://github.com/Homebrew/brew/pull/20804
[pkgxpantry]: https://docs.pkgx.sh/appendix/packaging/pantry
