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
