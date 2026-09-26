package build

import (
	"strings"
	"testing"
)

// The link set is what the artefact is built against, and nothing else: no
// build dependency and no base toolchain reaches it.
func TestEvalLinkDepsIsRuntimeOnly(t *testing.T) {
	got := EvalLinkDeps(map[string]any{"zlib.net": "^1", "unicode.org": "^71"}, lin())
	if len(got) != 2 || !contains(got, "zlib.net^1") || !contains(got, "unicode.org^71") {
		t.Fatalf("link deps = %v", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "gnu.org/autoconf") || strings.HasPrefix(s, "nodejs.org") {
			t.Errorf("a tool reached the link set: %q", s)
		}
	}
}

// The tool set is what the build runs: its build dependencies and the base
// toolchain.
func TestEvalToolDepsIsBuildPlusToolchain(t *testing.T) {
	got := EvalToolDeps("acme.org/thing", nil, map[string]any{"nodejs.org": "*"}, lin())
	if !contains(got, "nodejs.org") {
		t.Errorf("build dependency missing: %v", got)
	}
	if !contains(got, "gnu.org/autoconf") {
		t.Errorf("base toolchain missing: %v", got)
	}
}

// qt.io's shape, which one closure could not resolve: it LINKS unicode.org ^71
// and its build RUNS nodejs.org, whose own runtime needs ^73. The two sets must
// not mention each other, so the two resolutions never have to agree.
func TestTheQtShapeSplitsCleanly(t *testing.T) {
	runtime := map[string]any{"unicode.org": "^71", "freetype.org": "*"}
	buildD := map[string]any{"nodejs.org": "*", "python.org": ">=2.7"}

	link := EvalLinkDeps(runtime, lin())
	tools := EvalToolDeps("qt.io", runtime, buildD, lin())

	if !contains(link, "unicode.org^71") {
		t.Fatalf("link set lost qt's own constraint: %v", link)
	}
	if contains(link, "nodejs.org") {
		t.Errorf("the build tool reached the link set: %v", link)
	}
	if !contains(tools, "nodejs.org") {
		t.Errorf("tool set lost nodejs: %v", tools)
	}
	for _, s := range tools {
		if SpecProject(s) == "unicode.org" {
			t.Errorf("qt's link constraint reached the tool set as %q — nodejs would have to agree with it", s)
		}
	}

	// The control: the single-closure form this replaces puts both in one list,
	// which is what forced `^71` and nodejs's `^73` through one intersection.
	one := EvalDeps("qt.io", runtime, buildD, lin())
	if !contains(one, "unicode.org^71") || !contains(one, "nodejs.org") {
		t.Fatalf("control did not reproduce the single closure: %v", one)
	}
}

// Splitting must not turn the toolchain OVERRIDE into a coexistence: a recipe
// that names a toolchain project keeps its own, and the toolchain's copy is
// dropped rather than installed beside it.
func TestARecipesOwnToolchainPinStillReplacesTheBaseOne(t *testing.T) {
	// named in the runtime deps
	tools := EvalToolDeps("acme.org/thing", map[string]any{"perl.org": "^5.99"}, nil, lin())
	if contains(tools, "perl.org"+ToolchainPerl) {
		t.Errorf("the base perl survived a recipe that names its own: %v", tools)
	}
	if link := EvalLinkDeps(map[string]any{"perl.org": "^5.99"}, lin()); !contains(link, "perl.org^5.99") {
		t.Errorf("the recipe's own perl is missing from the link set: %v", link)
	}

	// named in the build deps
	tools = EvalToolDeps("acme.org/thing", nil, map[string]any{"gnu.org/gawk": "^5.4"}, lin())
	if contains(tools, "gnu.org/gawk"+ToolchainGawk) {
		t.Errorf("the base gawk survived a recipe that names its own: %v", tools)
	}
	if !contains(tools, "gnu.org/gawk^5.4") {
		t.Errorf("the recipe's own gawk is missing: %v", tools)
	}

	// and the project itself never joins its own toolchain
	if contains(EvalToolDeps("gnu.org/grep", nil, nil, lin()), "gnu.org/grep") {
		t.Error("a project's own published bottle joined the toolchain of its build")
	}
}

// TestBootstrapToolDepsDropsTheToolchainAndNothingElse.
//
// The base toolchain is what makes a first fill on a new architecture
// impossible: m4 declares no build dependencies and still fails on
// freedesktop.org/pkg-config, because BaseToolchain is injected into every
// build. Bootstrap mode drops exactly that, and must drop nothing else -- the
// recipe's OWN build dependencies are still bottles, deliberately.
func TestBootstrapToolDepsDropsTheToolchainAndNothingElse(t *testing.T) {
	// The real case: a recipe with no build dependencies of its own.
	if got := BootstrapToolDeps(nil, lin()); len(got) != 0 {
		t.Errorf("a recipe declaring nothing must ask for nothing, got %v", got)
	}
	// The control, same inputs, ordinary mode: the whole toolchain arrives.
	full := EvalToolDeps("gnu.org/m4", nil, nil, lin())
	if !contains(full, "freedesktop.org/pkg-config") {
		t.Fatalf("premise wrong: the toolchain no longer brings pkg-config: %v", full)
	}

	// And what the recipe names for itself survives.
	got := BootstrapToolDeps(map[string]any{"nodejs.org": "*"}, lin())
	if len(got) != 1 || SpecProject(got[0]) != "nodejs.org" {
		t.Errorf("BootstrapToolDeps = %v, want nodejs.org alone", got)
	}
}

// TestBaseToolchainHasNoEntryPoint is the finding itself, asserted rather than
// described: EVERY member of the base toolchain is handed the others, so there
// is no member that can be built first. A future toolchain with an entry point
// would make --bootstrap unnecessary, and this test is how that would be
// noticed rather than assumed.
func TestBaseToolchainHasNoEntryPoint(t *testing.T) {
	for _, spec := range BaseToolchain() {
		proj := SpecProject(spec)
		// Built in the ordinary way, with no recipe-declared deps at all.
		tools := EvalToolDeps(proj, nil, nil, lin())
		var others []string
		for _, s := range tools {
			if SpecProject(s) != proj {
				others = append(others, SpecProject(s))
			}
		}
		if len(others) == 0 {
			t.Errorf("%s needs nothing else: the toolchain HAS an entry point now, "+
				"so --bootstrap may no longer be the only way in", proj)
		}
	}
}
