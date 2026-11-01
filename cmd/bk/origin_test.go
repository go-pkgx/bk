package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgx/bk/build"
	"github.com/go-pkgx/bottle"
)

// originRefs builds a real provenance referrer through the production path, so
// these tests read what `bk publish` actually writes rather than a fixture
// somebody has to remember to update.
func originRefs(t *testing.T, o provOrigin) []bottle.Referrer {
	t.Helper()
	refs, err := buildReferrers("acme.org/tool", "1.2.3", "linux", "s390x",
		[]byte("tarball"), build.SourceRef{}, nil, time.Unix(0, 0).UTC(), nil, o)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

// ⛔ TestAnUnstatedOriginIsNotPromotable is the whole point of the tri-state.
// A `bool` would have made this case indistinguishable from "not a
// bootstrap", so every bottle published before the field existed would have
// claimed to be promotable — and the failure directions are not symmetric:
// refusing a good bottle costs a rebuild, promoting a host-built one puts an
// unpinned Ubuntu compiler's output in the production registry under our
// signature.
func TestAnUnstatedOriginIsNotPromotable(t *testing.T) {
	_, err := originAttestedBy(originRefs(t, provOrigin{}))
	if err == nil {
		t.Fatal("a provenance that says nothing about the origin read as an answer")
	}
	if !strings.Contains(err.Error(), "no build origin") {
		t.Errorf("the error does not say the origin is absent: %v", err)
	}
}

// ⛔ TestABootstrapBuildSaysSoAndIsRefused: this is the bottle that must never
// be promoted, and the recording is what makes refusing it possible at all.
func TestABootstrapBuildSaysSoAndIsRefused(t *testing.T) {
	o, err := originAttestedBy(originRefs(t, provOrigin{Bootstrap: "1", Dist: "oci://127.0.0.1:5000/seed"}))
	if err != nil {
		t.Fatal(err)
	}
	if o.Bootstrap != "1" {
		t.Errorf("Bootstrap = %q, want 1", o.Bootstrap)
	}
	if o.Dist != "oci://127.0.0.1:5000/seed" {
		t.Errorf("Dist = %q, want the loopback seed registry", o.Dist)
	}
	// And the refusal has to say WHY, naming the host distribution: "not
	// promotable" alone sends the reader back to the code.
	if why := o.why(); !strings.Contains(why, "bootstrap") || !strings.Contains(why, "host") {
		t.Errorf("why() = %q, want it to name --bootstrap and the host", why)
	}
}

// TestASovereignBuildIsPromotable: the positive case, which has to work or the
// gate refuses everything and nothing is ever promoted.
func TestASovereignBuildIsPromotable(t *testing.T) {
	o, err := originAttestedBy(originRefs(t, provOrigin{Bootstrap: "0", Dist: "oci://ghcr.io/go-pkgx/packages"}))
	if err != nil {
		t.Fatal(err)
	}
	if o.Bootstrap != "0" {
		t.Fatalf("Bootstrap = %q, want 0", o.Bootstrap)
	}
}

// ⛔ TestTheDistAloneIsNotAVerdict. A mirrored bottle records where it was PUT
// and nothing about who compiled it, so `Dist` is readable while `Bootstrap`
// stays empty — and that must NOT pass the gate. Recording a field is not the
// same as answering the question the gate asks.
func TestTheDistAloneIsNotAVerdict(t *testing.T) {
	o, err := originAttestedBy(originRefs(t, provOrigin{Dist: "oci://ghcr.io/go-pkgx/packages"}))
	if err != nil {
		t.Fatalf("a recorded dist should still parse: %v", err)
	}
	if o.Bootstrap == "0" {
		t.Fatal("an unrecorded bootstrap read as 0, which would promote a mirrored bottle")
	}
	if why := o.why(); !strings.Contains(why, "not say") {
		t.Errorf("why() = %q, want it to say the provenance is silent", why)
	}
}

// TestTheOriginKeysAreAbsentWhenUnstated. The provenance block already carries
// the lesson that a field which is always empty teaches readers to ignore it,
// so an unstated origin must emit no key at all rather than an empty one.
func TestTheOriginKeysAreAbsentWhenUnstated(t *testing.T) {
	ext := externalParamsOf(t, originRefs(t, provOrigin{}))
	for _, k := range []string{"bootstrap", "inputs-from"} {
		if v, ok := ext[k]; ok {
			t.Errorf("externalParameters[%q] = %q, want the key absent", k, v)
		}
	}
	// The pre-existing keys must survive: this change adds to the block and
	// does not rewrite it.
	for _, k := range []string{"project", "version", "platform"} {
		if _, ok := ext[k]; !ok {
			t.Errorf("externalParameters lost %q", k)
		}
	}
}

// TestAStatedOriginReachesTheAttestation, read out of the signed document
// rather than out of the struct that produced it.
func TestAStatedOriginReachesTheAttestation(t *testing.T) {
	ext := externalParamsOf(t, originRefs(t, provOrigin{Bootstrap: "0", Dist: "oci://ghcr.io/go-pkgx/packages"}))
	for k, want := range map[string]string{
		"bootstrap":   "0",
		"inputs-from": "oci://ghcr.io/go-pkgx/packages",
	} {
		if got := ext[k]; got != want {
			t.Errorf("externalParameters[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestBoolParamIsTheTwoKnownStates, and neither of them is the empty string —
// which is what keeps "unknown" a third thing.
func TestBoolParamIsTheTwoKnownStates(t *testing.T) {
	if got := boolParam(true); got != "1" {
		t.Errorf("boolParam(true) = %q, want 1", got)
	}
	if got := boolParam(false); got != "0" {
		t.Errorf("boolParam(false) = %q, want 0", got)
	}
}

// externalParamsOf digs the externalParameters block out of an in-toto
// referrer.
func externalParamsOf(t *testing.T, refs []bottle.Referrer) map[string]string {
	t.Helper()
	var raw []byte
	for _, r := range refs {
		if r.ArtifactType == artifactInToto {
			raw = r.Blob
			break
		}
	}
	if raw == nil {
		t.Fatal("no in-toto referrer was produced")
	}
	var stmt struct {
		Predicate struct {
			BuildDefinition struct {
				ExternalParameters map[string]string `json:"externalParameters"`
			} `json:"buildDefinition"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(raw, &stmt); err != nil {
		t.Fatal(err)
	}
	return stmt.Predicate.BuildDefinition.ExternalParameters
}

// TestOriginRefusesWithoutBothArguments: a project with no version would
// otherwise read the newest, and "the newest" is not the bottle somebody is
// deciding whether to promote.
func TestOriginRefusesWithoutBothArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runOrigin([]string{"acme.org/tool"}, &out, &errOut); code != 2 {
		t.Errorf("runOrigin with one argument = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Errorf("it does not print a usage line: %q", errOut.String())
	}
}
