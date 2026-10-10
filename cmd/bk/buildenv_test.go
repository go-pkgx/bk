package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bottle"
)

// provWith builds an in-toto referrer whose resolvedDependencies hold exactly
// the given URIs — the wire shape, so the reader is tested against what a
// registry serves and not against a struct we share with the writer.
func provWith(uris ...map[string]any) bottle.Referrer {
	deps := make([]map[string]any, 0, len(uris))
	deps = append(deps, uris...)
	b, _ := json.Marshal(map[string]any{
		"predicate": map[string]any{
			"buildDefinition": map[string]any{"resolvedDependencies": deps},
		},
	})
	return bottle.Referrer{ArtifactType: artifactInToto, Blob: b}
}

// ⛔⛔ "RECORDS NOTHING" AND "COULD NOT BE READ" ARE DIFFERENT ANSWERS.
// Every bottle published before bk v0.17.0 records no build environment, so a
// reader that returned a plain error for both would report the whole registry
// as broken — and a sweep built on it would be useless for deciding anything.
func TestEnvAttestedBySeparatesAbsenceFromFailure(t *testing.T) {
	if _, err := envAttestedBy(nil); !errors.Is(err, errNoProvenance) {
		t.Errorf("no attestation at all gave %v, want errNoProvenance", err)
	}
	_, err := envAttestedBy([]bottle.Referrer{provWith()})
	if !errors.Is(err, errNoEnvironment) {
		t.Errorf("an attestation with no environment gave %v, want errNoEnvironment", err)
	}
	// Malformed JSON is neither: it is a failure, and must not read as an
	// absence.
	bad := []bottle.Referrer{{ArtifactType: artifactInToto, Blob: []byte("{not json")}}
	_, err = envAttestedBy(bad)
	if err == nil || errors.Is(err, errNoEnvironment) || errors.Is(err, errNoProvenance) {
		t.Errorf("malformed provenance gave %v, want a parse failure", err)
	}
}

// IT READS THE ENVIRONMENT AND IGNORES THE SOURCE, which shares the list. The
// two are told apart by shape: a source material carries a sha256 of fetched
// bytes, an environment material carries a purl and no digest.
func TestEnvAttestedByReadsOnlyTheEnvironmentEntries(t *testing.T) {
	// ⛔ THE SOURCE URI HOLDS AN "@" ON PURPOSE. Written without one, this
	// fixture let a mutation that dropped the purl-prefix check SURVIVE: a
	// URI with no "@" is discarded by the version split anyway, so the test
	// passed for a second reason and measured nothing. A real source URI can
	// carry one — `https://user@host/...`, a tag named with one — and then
	// the prefix check is the only thing standing between a source archive
	// and being counted as a compiler.
	refs := []bottle.Referrer{provWith(
		map[string]any{"uri": "https://dl@example.com/jq-1.8.2.tar.gz", "digest": map[string]string{"sha256": "abc"}},
		map[string]any{"uri": "pkg:pkgx/go.dev@1.27.2"},
		map[string]any{"uri": "pkg:pkgx/gnu.org/glibc@2.44.0"},
	)}
	env, err := envAttestedBy(refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 {
		t.Fatalf("read %d entries, want 2: %+v", len(env), env)
	}
	// Sorted, so two bottles of the same closure compare as text.
	if env[0].Project != "gnu.org/glibc" || env[1].Project != "go.dev" {
		t.Errorf("not sorted by project: %+v", env)
	}
	if env[1].Version != "1.27.2" {
		t.Errorf("go.dev version = %q", env[1].Version)
	}
	// ⛔ THE SOURCE ARCHIVE IS NOT AN ENVIRONMENT ENTRY. Without this the
	// reader would count it and report an environment where there is none.
	for _, e := range env {
		if strings.Contains(e.Project, "example.com") {
			t.Errorf("the source archive was read as an environment entry: %+v", env)
		}
	}
}

// ⛔ THE LAST "@", because a project name is a path and a version never
// contains one. Cutting at the first would split pkg:pkgx/x@1@2 in the wrong
// place.
func TestEnvAttestedBySplitsAtTheLastAt(t *testing.T) {
	env, err := envAttestedBy([]bottle.Referrer{provWith(
		map[string]any{"uri": "pkg:pkgx/weird@name@1.2.3"},
	)})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0].Project != "weird@name" || env[0].Version != "1.2.3" {
		t.Errorf("split wrongly: %+v", env)
	}
	// And an entry with nothing after the "@", or nothing before it, is not
	// an entry: it is dropped rather than recorded as half of one.
	for _, u := range []string{"pkg:pkgx/x@", "pkg:pkgx/@1.2.3"} {
		if e, _ := envAttestedBy([]bottle.Referrer{provWith(map[string]any{"uri": u})}); len(e) != 0 {
			t.Errorf("%q produced %+v", u, e)
		}
	}
}

// ⛔⛔ A SWEEP MUST NOT FOLD "UNREADABLE" INTO "RECORDS NOTHING". An
// unreachable registry has told us nothing, and counting it as an absence is
// the same defect as a census that could not read reporting zero.
func TestTheSweepCountsTheThreeOutcomesSeparately(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "l.txt")
	if err := os.WriteFile(list, []byte("a.org 1\n# a comment\n\nb.org 2\nc.org 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := buildEnvOf
	t.Cleanup(func() { buildEnvOf = old })
	buildEnvOf = func(_, proj, _, _, _ string) ([]attestedEnv, error) {
		switch proj {
		case "a.org":
			return []attestedEnv{{Project: "go.dev", Version: "1.27.2"}}, nil
		case "b.org":
			return nil, errNoEnvironment
		default:
			return nil, errors.New("dial tcp: network is unreachable")
		}
	}
	var out, errb strings.Builder
	code := sweepBuildEnv(list, "oci://x", "linux", "aarch64", &out, &errb)
	if code != 1 {
		t.Errorf("code = %d, want 1 (something is not recorded)", code)
	}
	got := out.String()
	if !strings.Contains(got, "3 read · 1 record a build environment · 1 record none · 1 unreadable") {
		t.Errorf("the three outcomes are not counted separately:\n%s", got)
	}
	if !strings.Contains(got, "ERROR   c.org") {
		t.Errorf("the unreadable one is not named as an error:\n%s", got)
	}
}

// AN EMPTY LIST IS A FAILURE, not a clean registry — the same rule gopin
// states for its own corpus.
func TestTheSweepRefusesAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "l.txt")
	if err := os.WriteFile(list, []byte("# nothing but a comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := sweepBuildEnv(list, "oci://x", "linux", "aarch64", &out, &errb); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "not an empty registry") {
		t.Errorf("the refusal does not say why:\n%s", errb.String())
	}
}

// A LINE THE READER CANNOT USE IS NAMED, not skipped: a silent skip makes the
// totals smaller for a reason nobody can see.
func TestAMalformedLineIsNamedRatherThanSkipped(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "l.txt")
	if err := os.WriteFile(list, []byte("a.org 1\nb.org\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readProjectVersions(list)
	if err == nil {
		t.Fatal("a line with one field was accepted")
	}
	if !strings.Contains(err.Error(), "l.txt:2") || !strings.Contains(err.Error(), "b.org") {
		t.Errorf("the refusal locates nothing: %v", err)
	}
}

// THE COMMAND ITSELF, through the dispatch a user reaches. The repository
// gates at 100% coverage, and a CLI entry nobody exercises is exactly where a
// flag parses wrongly or an exit status is lost.
func TestBuildEnvCommand(t *testing.T) {
	old := buildEnvOf
	t.Cleanup(func() { buildEnvOf = old })
	buildEnvOf = func(_, proj, _, _, _ string) ([]attestedEnv, error) {
		if proj == "missing.org" {
			return nil, errNoEnvironment
		}
		return []attestedEnv{{Project: "go.dev", Version: "1.27.2"}}, nil
	}

	t.Run("one bottle, environment recorded", func(t *testing.T) {
		code, out, _ := run2(t, "buildenv", "--platform", "linux/aarch64", "a.org", "1.2.3")
		if code != 0 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(out, "go.dev 1.27.2") {
			t.Errorf("the entry is not printed:\n%s", out)
		}
		// The totals, as everywhere else here: a command that prints a list
		// and no count leaves the reader counting lines.
		if !strings.Contains(out, "1 entry(ies) recorded") {
			t.Errorf("no total:\n%s", out)
		}
	})

	// ⛔ AN ABSENCE EXITS NON-ZERO AND SAYS WHICH BOTTLE. A script asking
	// "is this comparable?" reads the status.
	t.Run("one bottle, nothing recorded", func(t *testing.T) {
		code, _, errb := run2(t, "buildenv", "--platform", "linux/aarch64", "missing.org", "1")
		if code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
		if !strings.Contains(errb, "missing.org") || !strings.Contains(errb, "no build environment") {
			t.Errorf("the refusal names neither the bottle nor the reason:\n%s", errb)
		}
	})

	t.Run("a platform that is not os/arch is refused", func(t *testing.T) {
		code, _, errb := run2(t, "buildenv", "--platform", "linux", "a.org", "1")
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if !strings.Contains(errb, "is not os/arch") {
			t.Errorf("the refusal does not say why:\n%s", errb)
		}
	})

	// Two arguments are needed, and the usage names BOTH forms — a reader
	// who wanted the sweep should not have to guess that it exists.
	t.Run("wrong argument count prints both usages", func(t *testing.T) {
		code, _, errb := run2(t, "buildenv", "--platform", "linux/aarch64", "a.org")
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if !strings.Contains(errb, "--list") {
			t.Errorf("the usage hides the sweep form:\n%s", errb)
		}
	})

	t.Run("an unknown flag is refused", func(t *testing.T) {
		if code, _, _ := run2(t, "buildenv", "--nope"); code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
	})

	t.Run("the sweep runs through the command", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "l.txt")
		if err := os.WriteFile(list, []byte("a.org 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, _ := run2(t, "buildenv", "--platform", "linux/aarch64", "--list", list)
		if code != 0 {
			t.Fatalf("code = %d out=%q", code, out)
		}
		if !strings.Contains(out, "1 read · 1 record a build environment") {
			t.Errorf("the sweep did not report:\n%s", out)
		}
	})

	// A list that cannot be READ is not a list that is empty.
	t.Run("an unreadable list is a refusal, not an empty sweep", func(t *testing.T) {
		code, _, errb := run2(t, "buildenv", "--platform", "linux/aarch64", "--list",
			filepath.Join(t.TempDir(), "absent.txt"))
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if strings.Contains(errb, "not an empty registry") {
			t.Errorf("an unreadable list was reported as an empty one:\n%s", errb)
		}
	})
}

// WITH NO --platform IT ASKS THE MACHINE, which is the default a person gets.
func TestBuildEnvDefaultsToThisMachine(t *testing.T) {
	old := buildEnvOf
	t.Cleanup(func() { buildEnvOf = old })
	var gotOS, gotArch string
	buildEnvOf = func(_, _, _, osn, arch string) ([]attestedEnv, error) {
		gotOS, gotArch = osn, arch
		return []attestedEnv{{Project: "go.dev", Version: "1.27.2"}}, nil
	}
	if code, _, errb := run2(t, "buildenv", "a.org", "1.2.3"); code != 0 {
		t.Fatalf("code = %d err=%q", code, errb)
	}
	if gotOS == "" || gotArch == "" {
		t.Errorf("the default platform resolved to %q/%q", gotOS, gotArch)
	}
}

// ⛔ AND WHEN THE MACHINE CANNOT BE RESOLVED, it says so instead of reading
// the registry for a platform it invented.
//
// No seam for this: target.Resolve() reads BREWKIT_TARGET, and a platform bk
// does not support makes the real function fail — the same case that once made
// `bk lint` refuse to run. A fake would test the fake.
func TestBuildEnvRefusesAnUnsupportedTarget(t *testing.T) {
	old := buildEnvOf
	t.Cleanup(func() { buildEnvOf = old })
	reached := false
	buildEnvOf = func(_, _, _, _, _ string) ([]attestedEnv, error) {
		reached = true
		return nil, nil
	}
	t.Setenv("BREWKIT_TARGET", "plan9/pdp11")

	var out, errb strings.Builder
	// Not through run2: it clears BREWKIT_TARGET on purpose, which is the
	// right thing for every other test here and the wrong thing for this one.
	code := runBuildEnv([]string{"a.org", "1.2.3"}, &out, &errb)
	if code != 2 {
		t.Errorf("code = %d, want 2; stderr=%q", code, errb.String())
	}
	if reached {
		t.Error("it read the registry for a platform it could not resolve")
	}
	if errb.Len() == 0 {
		t.Error("it refused in silence")
	}
}
