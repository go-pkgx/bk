package build

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/go-pkgx/bk/buildscript"
	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/fixup"
	"github.com/go-pkgx/bk/moustache"
	"github.com/go-pkgx/bk/pantry"
	"github.com/go-pkgx/bk/target"
)

// Runner drives a full build. Its side-effecting steps are fields so the whole
// orchestration is testable with stubs; NewRunner wires the real bk packages.
type Runner struct {
	PickVersion func(project, constraint string) (string, error)
	// ResolveVersion resolves the MAIN project version from the recipe's own
	// `versions:` spec, so it matches the distributable URL's {{version.raw}}
	// rather than dist's already-built (possibly normalised) bottle list.
	ResolveVersion func(spec any, constraint string) (string, error)
	Fetch          func(url, dest string, strip int) error
	FetchGit       func(repo, ref, dest string) error
	Touch          func(dir string) error
	Run            func(scriptPath string, env []string) error
	FixUp          func(fixup.Options) error
	WriteBottle    func(installDir, project, version, osn, arch, outDir string) (string, error)
	// ResolveDep, when set, resolves each dependency to a version so the build
	// gets {{deps.<project>.prefix}}/{{deps.<project>.version}} tokens.
	ResolveDep  func(project, constraint string) (string, error)
	Concurrency int
	PkgxBin     string
	BashPath    string
	// RecipeDir is the pantry project directory holding the recipe (package.yml)
	// and its sibling files. It is copied into the build tree as `props/` so
	// recipes can reference those files as `props/foo` or via the {{props}}
	// moustache (pkgx's convention). Empty disables the behaviour.
	RecipeDir string
}

// Result reports what a build produced.
type Result struct {
	Version    string
	Install    string
	ScriptPath string
	BottlePath string
}

// Build runs the pipeline for project@constraint built for tgt (running on host)
// and, when distOut != "", packages a bottle there.
func (r *Runner) Build(recipe *pantry.Recipe, project, constraint string, tgt, host target.Target, distOut string) (Result, error) {
	var res Result
	version, err := r.ResolveVersion(recipe.Versions, constraint)
	if err != nil {
		return res, fmt.Errorf("resolve version: %w", err)
	}
	res.Version = version

	paths := config.Compute(project, version, tgt)
	res.Install = paths.Install
	for _, d := range []string{paths.Build, paths.Install, paths.BuildInstall} {
		if err := osRemoveAll(d); err != nil {
			return res, err
		}
	}
	if err := osMkdirAll(paths.Build, 0o755); err != nil {
		return res, err
	}
	if err := osMkdirAll(paths.Home, 0o755); err != nil {
		return res, err
	}

	// fetch source
	if recipe.Distributable != nil {
		src, err := sourceOf(recipe.Distributable, version)
		if err != nil {
			return res, err
		}
		if src.git {
			err = r.FetchGit(src.url, src.ref, paths.Build)
		} else {
			err = r.Fetch(src.url, paths.Build, src.strip)
		}
		if err != nil {
			return res, fmt.Errorf("fetch: %w", err)
		}
		if err := r.Touch(paths.Build); err != nil {
			return res, err
		}
	}

	// Expose the recipe's own directory as `props/` in the build tree — pkgx's
	// convention: a recipe references its sibling files (patches, helper scripts)
	// as `props/foo` or via the {{props}} moustache, where `props` IS the recipe
	// directory (e.g. gnu.org/tar's props/iconv.patch is projects/gnu.org/tar/
	// iconv.patch). Copy the whole recipe dir (package.yml included, harmless).
	propsDir := filepath.Join(paths.Build, "props")
	if r.RecipeDir != "" {
		if _, err := osStat(r.RecipeDir); err == nil {
			if err := copyProps(r.RecipeDir, propsDir); err != nil {
				return res, fmt.Errorf("copy props: %w", err)
			}
		}
	}

	// deps + tokens + script
	deps := EvalDeps(recipe.Dependencies, buildDeps(recipe), tgt)
	toks := moustache.Prefix(paths.BuildInstall)
	toks = append(toks, moustache.Version(version, "version")...)
	toks = append(toks, moustache.Host(tgt.Arch, tgt.Triple, tgt.Platform, r.concurrency())...)
	toks = append(toks,
		moustache.Token{From: "srcroot", To: paths.Build},
		moustache.Token{From: "props", To: propsDir},
		moustache.Token{From: "pkgx.prefix", To: config.PkgxDir()},
	)
	if r.ResolveDep != nil {
		dt, err := DepTokens(recipe.Dependencies, buildDeps(recipe), tgt, config.PkgxDir(), r.ResolveDep)
		if err != nil {
			return res, fmt.Errorf("resolve deps: %w", err)
		}
		toks = append(toks, dt...)
	}
	user, err := buildscript.Generate(recipe.Build, buildscript.Options{Target: tgt, PkgVersion: version, Tokens: toks})
	if err != nil {
		return res, fmt.Errorf("generate: %w", err)
	}
	script := buildscript.Wrap(buildscript.WrapOptions{
		UserScript: user, Deps: deps, Target: tgt, Host: host,
		Home: paths.Home, SrcRoot: paths.Build, PkgxDir: config.PkgxDir(),
		PkgxBin: r.PkgxBin, BashPath: r.BashPath,
	})
	res.ScriptPath = paths.Build + ".sh"
	if err := osWriteFile(res.ScriptPath, []byte(script), 0o755); err != nil {
		return res, err
	}

	// run (sanitized env), stage, fix-up
	if err := r.Run(res.ScriptPath, SanitizedEnv(paths.Home, config.PkgxDir())); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	if err := osRename(paths.BuildInstall, paths.Install); err != nil {
		return res, fmt.Errorf("stage install: %w", err)
	}
	if err := r.FixUp(fixup.Options{Prefix: paths.Install, BuildInstall: paths.BuildInstall, Platform: tgt.Platform}); err != nil {
		return res, fmt.Errorf("fix-up: %w", err)
	}

	// package
	if distOut != "" {
		p, err := r.WriteBottle(paths.Install, project, version, tgt.Platform, tgt.Arch, distOut)
		if err != nil {
			return res, fmt.Errorf("package: %w", err)
		}
		res.BottlePath = p
	}
	return res, nil
}

func (r *Runner) concurrency() int {
	if r.Concurrency > 0 {
		return r.Concurrency
	}
	return runtime.NumCPU()
}

func buildDeps(recipe *pantry.Recipe) map[string]any {
	if m, ok := recipe.Build.(map[string]any); ok {
		if bd, ok := m["dependencies"].(map[string]any); ok {
			return bd
		}
	}
	return nil
}

type source struct {
	url   string
	strip int
	git   bool
	ref   string
}

// sourceOf derives the fetch source from a recipe's distributable, applying the
// version moustaches to the URL.
func sourceOf(dist any, version string) (source, error) {
	toks := moustache.Version(version, "version")
	switch d := dist.(type) {
	case string:
		return source{url: moustache.Apply(d, toks)}, nil
	case map[string]any:
		s := source{url: moustache.Apply(str(d["url"]), toks)}
		if ref, ok := d["ref"].(string); ok && ref != "" {
			s.git, s.ref = true, moustache.Apply(ref, toks)
		}
		switch sc := d["strip-components"].(type) {
		case int:
			s.strip = sc
		case string:
			s.strip, _ = strconv.Atoi(sc)
		}
		if s.url == "" {
			return s, fmt.Errorf("distributable has no url")
		}
		return s, nil
	case []any:
		// A sequence lists the canonical distributable first, with fallback
		// mirrors after (e.g. openssl.org ships the upstream tarball plus a
		// GitHub-release mirror). The primary entry is authoritative, so derive
		// the source from it.
		if len(d) == 0 {
			return source{}, fmt.Errorf("distributable list is empty")
		}
		return sourceOf(d[0], version)
	default:
		return source{}, fmt.Errorf("unsupported distributable %T", dist)
	}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// copyPropsTree recursively copies src into dst, preserving each file's
// permission bits (patches ship 0644, helper scripts may be executable). Every
// filesystem call goes through an os seam so all error branches are testable.
func copyPropsTree(src, dst string) error {
	info, err := osStat(src)
	if err != nil {
		return err
	}
	if err := osMkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	entries, err := osReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		sp, dp := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyPropsTree(sp, dp); err != nil {
				return err
			}
			continue
		}
		fi, err := osStat(sp)
		if err != nil {
			return err
		}
		data, err := osReadFile(sp)
		if err != nil {
			return err
		}
		if err := osWriteFile(dp, data, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
