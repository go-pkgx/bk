package main

import (
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bottle"
)

// decodeProv pulls the in-toto statement out of the referrers and reads it as
// the wire shape a consumer sees, not as the struct we happened to build.
func decodeProv(t *testing.T, refs []bottle.Referrer) map[string]any {
	t.Helper()
	for _, r := range refs {
		if r.ArtifactType != artifactInToto {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(r.Blob, &m); err != nil {
			t.Fatalf("the provenance is not JSON: %v", err)
		}
		return m
	}
	t.Fatal("no in-toto referrer was produced")
	return nil
}

// ⛔⛔ A BUILDER THAT NEVER CHANGES CANNOT TELL TWO BUILDS APART.
//
// Measured 2026-10-09 on the published stedolan.github.io/jq: its signed SLSA
// statement carries
//
//	"builder": { "id": "https://github.com/go-pkgx/bk" }
//	"buildDefinition": { "externalParameters": {}, "internalParameters": {} }
//
// — the same two constants every bottle in the registry carries. So "which bk
// built this, and for what?" had no answer in the artefact, and a rebuild for
// a toolchain security fix was indistinguishable from the build before it.
func TestTheProvenanceSaysWhichBkAndWhatItWasAsked(t *testing.T) {
	refs, err := buildReferrers("acme.org/tool", "1.2.3", "linux", "aarch64",
		[]byte("tarball"), build.SourceRef{}, time.Unix(0, 0).UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeProv(t, refs)

	pred, _ := m["predicate"].(map[string]any)
	run, _ := pred["runDetails"].(map[string]any)
	b, _ := run["builder"].(map[string]any)
	id, _ := b["id"].(string)

	// ⛔ THE REGRESSION: the bare constant, with nothing after it.
	if id == builderID {
		t.Errorf("builder.id is the version-less constant again: %q", id)
	}
	if !strings.HasPrefix(id, builderID+"@") {
		t.Errorf("builder.id = %q, want %s@<version>", id, builderID)
	}
	if got := strings.TrimPrefix(id, builderID+"@"); got != bkVersion() {
		t.Errorf("builder.id names %q, but this bk is %q", got, bkVersion())
	}

	// AND THE QUESTION THE BUILD WAS ASKED, which was an empty object.
	def, _ := pred["buildDefinition"].(map[string]any)
	ext, _ := def["externalParameters"].(map[string]any)
	if len(ext) == 0 {
		t.Fatalf("externalParameters is still empty:\n%v", def)
	}
	for k, want := range map[string]string{
		"project":  "acme.org/tool",
		"version":  "1.2.3",
		"platform": "linux/aarch64",
	} {
		if got, _ := ext[k].(string); got != want {
			t.Errorf("externalParameters[%q] = %q, want %q", k, got, want)
		}
	}
}

// TWO DIFFERENT bk VERSIONS MUST PRODUCE DIFFERENT STATEMENTS. The point is
// not that a version appears, it is that the bytes differ — a reader diffing
// two attestations has to be able to see it.
func TestTwoBuildersProduceDifferentProvenance(t *testing.T) {
	// buildInfo is the seam bkVersion() reads, so two bk versions are two
	// answers from it rather than two binaries.
	old := buildInfo
	t.Cleanup(func() { buildInfo = old })
	at := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: v}}, true
		}
	}

	buildInfo = at("v0.16.0")
	a, err := buildReferrers("acme.org/tool", "1.2.3", "linux", "aarch64",
		[]byte("tarball"), build.SourceRef{}, time.Unix(0, 0).UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	buildInfo = at("v0.17.0")
	b, err := buildReferrers("acme.org/tool", "1.2.3", "linux", "aarch64",
		[]byte("tarball"), build.SourceRef{}, time.Unix(0, 0).UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := provBlob(t, a), provBlob(t, b)
	if string(pa) == string(pb) {
		t.Error("two bk versions produced byte-identical provenance")
	}
	if !strings.Contains(string(pa), "v0.16.0") || !strings.Contains(string(pb), "v0.17.0") {
		t.Errorf("neither statement names its own bk:\n%s\n---\n%s", pa, pb)
	}
}

func provBlob(t *testing.T, refs []bottle.Referrer) []byte {
	t.Helper()
	for _, r := range refs {
		if r.ArtifactType == artifactInToto {
			return r.Blob
		}
	}
	t.Fatal("no in-toto referrer")
	return nil
}
