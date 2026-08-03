package build

import (
	"fmt"
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
	Fetch       func(url, dest string, strip int) error
	FetchGit    func(repo, ref, dest string) error
	Touch       func(dir string) error
	Run         func(scriptPath string, env []string) error
	FixUp       func(fixup.Options) error
	WriteBottle func(installDir, project, version, osn, arch, outDir string) (string, error)
	// ResolveDep, when set, resolves each dependency to a version so the build
	// gets {{deps.<project>.prefix}}/{{deps.<project>.version}} tokens.
	ResolveDep  func(project, constraint string) (string, error)
	Concurrency int
	PkgxBin     string
	BashPath    string
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
	version, err := r.PickVersion(project, constraint)
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

	// deps + tokens + script
	deps := EvalDeps(recipe.Dependencies, buildDeps(recipe), tgt)
	toks := moustache.Prefix(paths.BuildInstall)
	toks = append(toks, moustache.Version(version, "version")...)
	toks = append(toks, moustache.Host(tgt.Arch, tgt.Triple, tgt.Platform, r.concurrency())...)
	toks = append(toks,
		moustache.Token{From: "srcroot", To: paths.Build},
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
