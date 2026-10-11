package vuln

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, y string) *AdvisoryFile {
	t.Helper()
	f, err := ParseAdvisories([]byte(y))
	if err != nil {
		t.Fatalf("ParseAdvisories: %v", err)
	}
	return f
}

func opts(subjects ...Subject) VEXOptions {
	return VEXOptions{
		ID:        "https://go-pkgx.dev/vex/1",
		Author:    "https://github.com/go-pkgx",
		Timestamp: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
		Version:   1,
		Subjects:  subjects,
	}
}

const product = "pkg:oci/openssl@sha256:abc?repository_url=ghcr.io/go-pkgx/openssl.org&tag=3.5.8_1--linux-x86-64"

// prod is the one-product case, named only so the tests that do not care about
// identifiers or hashes stay about what they test.
func prod(id string) []Product { return []Product{{ID: id}} }

// The document carries the five fields the spec marks required, and the
// context names the VERSION — the spec says an omitted version defaults to
// v0.0.1, so leaving it off is not a shorthand.
func TestVEXDocumentHasTheRequiredFields(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: openssl.org
advisories:
  - id: CVE-2026-0001
    aliases: [GHSA-xxxx]
    events:
      - timestamp: 2026-10-10T12:00:00Z
        type: fixed
        fixed-version: 3.5.8_1
`)
	b, err := EncodeVEX(opts(Subject{File: f, Products: prod(product)}))
	if err != nil {
		t.Fatalf("EncodeVEX: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	for _, k := range []string{"@context", "@id", "author", "timestamp", "version", "statements"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("required field %q is absent", k)
		}
	}
	if doc["@context"] != VEXContext || !strings.Contains(VEXContext, "v0.2.0") {
		t.Errorf("@context = %v, want a versioned context", doc["@context"])
	}
	sts, _ := doc["statements"].([]any)
	if len(sts) != 1 {
		t.Fatalf("statements = %v", sts)
	}
	st := sts[0].(map[string]any)
	if st["status"] != VEXFixed {
		t.Errorf("status = %v, want fixed", st["status"])
	}
	// ⛔ The STATEMENT's timestamp is when the event was known to be true, not
	// when the document was written. Dating every finding to the last publish
	// would make the whole history look like it was discovered at once.
	if st["timestamp"] != "2026-10-10T12:00:00Z" {
		t.Errorf("statement timestamp = %v, want the event's", st["timestamp"])
	}
	if doc["timestamp"] != "2026-10-11T09:00:00Z" {
		t.Errorf("document timestamp = %v, want the issue time", doc["timestamp"])
	}
	v := st["vulnerability"].(map[string]any)
	if v["name"] != "CVE-2026-0001" {
		t.Errorf("vulnerability = %v", v)
	}
	if al, _ := v["aliases"].([]any); len(al) != 1 {
		t.Errorf("aliases = %v, want the GHSA carried through", v["aliases"])
	}
	prods := st["products"].([]any)
	if len(prods) != 1 || prods[0].(map[string]any)["@id"] != product {
		t.Errorf("products = %v", prods)
	}
}

// The hash and the purl are what make a statement checkable rather than
// plausible — and the hash key's spelling is a trap: OpenVEX's table says
// `sha-256` while sha1 in the same table is `sha1`. `hashes` is a free-form
// map, so the wrong key is a digest nobody reads and no error.
func TestAProductCarriesItsPurlAndItsDigest(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: openssl.org
advisories:
  - id: CVE-2026-0001
    events:
      - timestamp: 2026-10-10T12:00:00Z
        type: fixed
        fixed-version: 3.5.8_1
`)
	b, err := EncodeVEX(opts(Subject{File: f, Products: []Product{{
		ID:     "pkg:pkgx/openssl.org@3.5.8_1",
		PURL:   "pkg:pkgx/openssl.org@3.5.8_1",
		SHA256: "deadbeef",
	}}}))
	if err != nil {
		t.Fatalf("EncodeVEX: %v", err)
	}
	var doc struct {
		Statements []struct {
			Products []struct {
				ID          string            `json:"@id"`
				Identifiers map[string]string `json:"identifiers"`
				Hashes      map[string]string `json:"hashes"`
			} `json:"products"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Statements[0].Products[0]
	if p.ID != "pkg:pkgx/openssl.org@3.5.8_1" {
		t.Errorf("@id = %q", p.ID)
	}
	if p.Identifiers["purl"] != "pkg:pkgx/openssl.org@3.5.8_1" {
		t.Errorf("identifiers = %v, want a purl", p.Identifiers)
	}
	if p.Hashes["sha-256"] != "deadbeef" {
		t.Errorf("hashes = %v, want the digest under %q", p.Hashes, "sha-256")
	}
	// And under NO other spelling, so a consumer cannot find it by accident.
	if _, ok := p.Hashes["sha256"]; ok {
		t.Errorf("the digest is also under %q: %v", "sha256", p.Hashes)
	}
}

// Both maps are omitted when there is nothing to put in them — a product
// carrying `"hashes": {}` claims to be hashed and is not.
func TestAProductWithoutIdentifiersOmitsTheMaps(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: p
advisories:
  - id: CVE-1
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	b, err := EncodeVEX(opts(Subject{File: f, Products: prod(product)}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "identifiers") || strings.Contains(string(b), "hashes") {
		t.Errorf("empty maps were emitted:\n%s", b)
	}
}

// A product with no @id is refused: the spec allows it, this does not, because
// a statement whose subject cannot be named is one nobody can look up.
func TestAProductWithoutAnIDIsRefused(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: p
advisories:
  - id: CVE-1
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	_, err := EncodeVEX(opts(Subject{File: f, Products: []Product{{SHA256: "deadbeef"}}}))
	if err == nil || !strings.Contains(err.Error(), "no @id") {
		t.Errorf("err = %v", err)
	}
}

// not_affected MUST carry a justification label. A document without one is
// rejected by consumers, and the prose form "breaks VEX automation".
func TestNotAffectedCarriesItsJustification(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: openssl.org
advisories:
  - id: CVE-2026-0002
    events:
      - timestamp: 2026-10-10T12:00:00Z
        type: false-positive-determination
        justification: vulnerable_code_not_present
        note: built without the affected backend
`)
	b, err := EncodeVEX(opts(Subject{File: f, Products: prod(product)}))
	if err != nil {
		t.Fatalf("EncodeVEX: %v", err)
	}
	var doc struct {
		Statements []struct {
			Status          string `json:"status"`
			Justification   string `json:"justification"`
			ImpactStatement string `json:"impact_statement"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Statements[0]
	if s.Status != VEXNotAffected || s.Justification != "vulnerable_code_not_present" {
		t.Errorf("statement = %+v", s)
	}
	// The prose rides ALONGSIDE the label, never instead of it.
	if s.ImpactStatement != "built without the affected backend" {
		t.Errorf("impact_statement = %q", s.ImpactStatement)
	}
}

// Each event type that becomes `affected` means something different to a
// reader, so each carries its own action statement.
func TestAffectedStatusesCarryDistinctActions(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []struct{ typ, body string }{
		{EventTruePositive, ""},
		{EventFixNotPlanned, ""},
		{EventPendingUpstreamFix, ""},
	} {
		y := "schema-version: 1\nproject: p\nadvisories:\n  - id: CVE-1\n    events:\n" +
			"      - timestamp: 2026-10-10T12:00:00Z\n        type: " + c.typ + "\n" + c.body
		b, err := EncodeVEX(opts(Subject{File: mustParse(t, y), Products: prod(product)}))
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		var doc struct {
			Statements []struct {
				Status          string `json:"status"`
				ActionStatement string `json:"action_statement"`
			} `json:"statements"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		s := doc.Statements[0]
		if s.Status != VEXAffected {
			t.Errorf("%s: status = %q, want affected", c.typ, s.Status)
		}
		if s.ActionStatement == "" {
			t.Errorf("%s: an affected statement with no action", c.typ)
		}
		if seen[s.ActionStatement] {
			t.Errorf("%s: shares its action statement with another event type", c.typ)
		}
		seen[s.ActionStatement] = true
	}
}

// A detection is a NAME match, not a finding. This fleet measured what a bare
// CPE match is worth — 27 records for openssl, four from 2009 — so emitting
// detections as `affected` would publish false alarms under our signature.
func TestDetectionIsUnderInvestigation(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: openssl.org
advisories:
  - id: CVE-2009-0590
    events:
      - timestamp: 2026-10-10T12:00:00Z
        type: detection
        note: matched by a bare CPE query, not yet triaged
`)
	b, err := EncodeVEX(opts(Subject{File: f, Products: prod(product)}))
	if err != nil {
		t.Fatalf("EncodeVEX: %v", err)
	}
	var doc struct {
		Statements []struct {
			Status      string `json:"status"`
			StatusNotes string `json:"status_notes"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Statements[0].Status != VEXUnderInvestigation {
		t.Errorf("status = %q, want under_investigation", doc.Statements[0].Status)
	}
	if doc.Statements[0].StatusNotes == "" {
		t.Error("the note is the only thing saying why it is undecided")
	}
}

// Every event type the parser accepts must map to a status. An unmapped one
// would silently drop its statement from the document.
func TestEveryEventTypeMapsToAStatus(t *testing.T) {
	if len(statusFor) != len(eventTypes) {
		t.Fatalf("%d event types, %d mapped to a VEX status", len(eventTypes), len(statusFor))
	}
	for typ := range eventTypes {
		if statusFor[typ] == "" {
			t.Errorf("%q maps to no status", typ)
		}
	}
	// And every status that requires an action statement has one.
	for typ, status := range statusFor {
		if status == VEXAffected && actionFor[typ] == "" {
			t.Errorf("%q is affected and has no action statement", typ)
		}
	}
}

// The same records produce the same bytes, whatever order they are handed in.
// A document that differed run to run could not be attached to a bottle and
// compared with the one already there.
func TestEncodingIsDeterministic(t *testing.T) {
	a := mustParse(t, `schema-version: 1
project: a.org
advisories:
  - id: CVE-2
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
  - id: CVE-1
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	b := mustParse(t, `schema-version: 1
project: b.org
advisories:
  - id: CVE-3
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	one, err := EncodeVEX(opts(
		Subject{File: b, Products: prod("pkg:oci/b@sha256:1")},
		Subject{File: a, Products: prod("pkg:oci/a@sha256:2")},
	))
	if err != nil {
		t.Fatal(err)
	}
	two, err := EncodeVEX(opts(
		Subject{File: a, Products: prod("pkg:oci/a@sha256:2")},
		Subject{File: b, Products: prod("pkg:oci/b@sha256:1")},
	))
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Errorf("the order the subjects were handed in changed the document:\n%s\n---\n%s", one, two)
	}
	// Ordered by project, then by vulnerability id within it.
	i1, i2, i3 := strings.Index(string(one), "CVE-1"), strings.Index(string(one), "CVE-2"), strings.Index(string(one), "CVE-3")
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("statements are not ordered by project then id: %d %d %d", i1, i2, i3)
	}
}

// Sorting must not reorder the CALLER's slices: a function that sorts its
// input in place changes a value the caller still holds.
func TestEncodingDoesNotReorderTheCallersData(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: a.org
advisories:
  - id: CVE-2
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
  - id: CVE-1
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	subjects := []Subject{{File: f, Products: prod("pkg:oci/a@sha256:2")}}
	if _, err := EncodeVEX(opts(subjects...)); err != nil {
		t.Fatal(err)
	}
	if f.Advisories[0].ID != "CVE-2" {
		t.Errorf("the advisory file was reordered under the caller: %s", f.Advisories[0].ID)
	}
}

func TestEncodeVEXRefusals(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: p
advisories:
  - id: CVE-1
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	good := Subject{File: f, Products: prod(product)}
	cases := []struct {
		name string
		mut  func(*VEXOptions)
		want string
	}{
		{"no @id", func(o *VEXOptions) { o.ID = " " }, "no @id"},
		{"no author", func(o *VEXOptions) { o.Author = "" }, "no author"},
		{"no timestamp", func(o *VEXOptions) { o.Timestamp = time.Time{} }, "no timestamp"},
		{"version zero", func(o *VEXOptions) { o.Version = 0 }, "it starts at 1"},
		{"no subjects", func(o *VEXOptions) { o.Subjects = nil }, "no subjects"},
		{"a subject with no file", func(o *VEXOptions) { o.Subjects = []Subject{{Products: prod(product)}} }, "no advisory file"},
		{"a subject with no product", func(o *VEXOptions) { o.Subjects = []Subject{{File: f}} }, "names no product"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := opts(good)
			c.mut(&o)
			_, err := EncodeVEX(o)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// An event type the parser allows but this file does not map is refused loudly
// rather than dropped. Reached by hand, because the parser and the mapping are
// kept level by TestEveryEventTypeMapsToAStatus — which is exactly why the
// error arm needs its own way in.
func TestAnUnmappedEventTypeIsRefused(t *testing.T) {
	a := Advisory{
		ID:     "CVE-1",
		Events: []Event{{Timestamp: time.Now(), Type: "invented-later"}},
	}
	if _, err := statementFor(a, prod(product)); err == nil || !strings.Contains(err.Error(), "maps to no VEX status") {
		t.Errorf("err = %v", err)
	}
}

// And the refusal reaches the caller of EncodeVEX, naming the project and the
// advisory. AdvisoryFile is an exported struct, so a caller really can build
// one without going through the parser — which is the only way an unmapped
// event type gets this far.
func TestAnUnmappedEventTypeFailsTheWholeDocument(t *testing.T) {
	f := &AdvisoryFile{
		SchemaVersion: AdvisoryVersion,
		Project:       "openssl.org",
		Advisories: []Advisory{{
			ID:     "CVE-1",
			Events: []Event{{Timestamp: time.Now(), Type: "invented-later"}},
		}},
	}
	_, err := EncodeVEX(opts(Subject{File: f, Products: prod(product)}))
	if err == nil {
		t.Fatal("emitted a document with a statement it could not write")
	}
	for _, want := range []string{"openssl.org", "CVE-1", "maps to no VEX status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The optional document fields appear when set and are absent when not — a
// consumer reading `"role": ""` sees a document asserting an empty role.
func TestOptionalDocumentFieldsAreOmittedWhenUnset(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: p
advisories:
  - id: CVE-1
    events: [{timestamp: 2026-10-10T12:00:00Z, type: detection}]
`)
	o := opts(Subject{File: f, Products: prod(product)})
	bare, err := EncodeVEX(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "\"role\"") || strings.Contains(string(bare), "\"tooling\"") {
		t.Errorf("unset optional fields were emitted:\n%s", bare)
	}
	o.Role = "Document Creator"
	o.Tooling = "bk"
	full, err := EncodeVEX(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(full), "Document Creator") || !strings.Contains(string(full), "\"tooling\": \"bk\"") {
		t.Errorf("set optional fields were dropped:\n%s", full)
	}
}
