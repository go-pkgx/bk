package vuln

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// osvServer answers a batch query with the given per-query result bodies, and
// records what was asked.
func osvServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var asked string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		asked = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s, &asked
}

func TestAskReturnsFindingsWithTheIdentityThatFoundThem(t *testing.T) {
	s, asked := osvServer(t, `{"results":[
		{"vulns":[{"id":"RUSTSEC-2021-0106","modified":"2026-02-04T02:31:56Z"}]},
		{}
	]}`)
	c := &OSVClient{Endpoint: s.URL}
	found, skipped, err := c.Ask([]Identity{
		{DB: "crates.io", Name: "bat"},
		{DB: "crates.io", Name: "bpb"},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v", skipped)
	}
	if len(found) != 1 {
		t.Fatalf("found = %v", found)
	}
	// ⛔ The IDENTITY, not just the id. With two keys per project, "which key
	// found this" is not recoverable afterwards and is the first thing a
	// person triaging it needs.
	if found[0].Identity.Name != "bat" || found[0].ID != "RUSTSEC-2021-0106" {
		t.Errorf("found = %+v", found[0])
	}
	if found[0].Modified == "" {
		t.Error("the modification time is dropped")
	}
	// The request names the ecosystem and the package, which is the only shape
	// OSV answers.
	var req osvRequest
	if err := json.Unmarshal([]byte(*asked), &req); err != nil {
		t.Fatalf("the request is not the documented shape: %v", err)
	}
	if len(req.Queries) != 2 || req.Queries[0].Package.Ecosystem != "crates.io" || req.Queries[0].Package.Name != "bat" {
		t.Errorf("asked %s", *asked)
	}
}

// ⛔ A repository identity is DROPPED BEFORE the request, not sent and found
// empty. Measured against the live API: {"package":{"purl":"pkg:github/curl/curl"}}
// returns {}, which is indistinguishable from "no known vulnerabilities".
// Sending them would report 1186 projects as clean.
func TestAskDoesNotAskWhatOSVCannotAnswer(t *testing.T) {
	s, asked := osvServer(t, `{"results":[{}]}`)
	c := &OSVClient{Endpoint: s.URL}
	found, skipped, err := c.Ask([]Identity{
		{DB: "GitHub", Name: "curl/curl"},
		{DB: "GitLab", Name: "g/p"},
		{DB: "cpe", Name: "openssl:openssl"},
		{DB: "npm", Name: "left-pad"},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("found = %v", found)
	}
	if len(skipped) != 3 {
		t.Fatalf("skipped = %v, want the three unanswerable ones", skipped)
	}
	if strings.Contains(*asked, "curl/curl") || strings.Contains(*asked, "openssl") {
		t.Errorf("an unanswerable identity was sent: %s", *asked)
	}
	if !strings.Contains(*asked, "left-pad") {
		t.Errorf("the answerable one was not sent: %s", *asked)
	}
}

func TestQueryable(t *testing.T) {
	for _, c := range []struct {
		id   Identity
		want bool
	}{
		{Identity{DB: "crates.io", Name: "x"}, true},
		{Identity{DB: "npm", Name: "x"}, true},
		{Identity{DB: "GitHub", Name: "o/r"}, false},
		{Identity{DB: "GitLab", Name: "o/r"}, false},
		{Identity{DB: "cpe", Name: "v:p"}, false},
		{Identity{DB: "invented", Name: "x"}, false},
	} {
		if got := Queryable(c.id); got != c.want {
			t.Errorf("Queryable(%v) = %v, want %v", c.id, got, c.want)
		}
	}
}

// ⛔ THE ANSWER IS POSITIONAL. OSV answers in the order asked and says nothing
// about which query each result belongs to, so a short response would
// attribute every finding from that point on to the wrong package.
func TestAskRefusesAMismatchedResultCount(t *testing.T) {
	s, _ := osvServer(t, `{"results":[{"vulns":[{"id":"X"}]}]}`)
	c := &OSVClient{Endpoint: s.URL}
	_, _, err := c.Ask([]Identity{{DB: "npm", Name: "a"}, {DB: "npm", Name: "b"}})
	if err == nil || !strings.Contains(err.Error(), "asked 2, answered 1") {
		t.Errorf("err = %v", err)
	}
}

func TestAskReportsTransportAndServerFailures(t *testing.T) {
	t.Run("a refused connection", func(t *testing.T) {
		c := &OSVClient{Endpoint: "http://127.0.0.1:1/"}
		if _, _, err := c.Ask([]Identity{{DB: "npm", Name: "a"}}); err == nil {
			t.Error("a refused connection was reported as no findings")
		}
	})
	t.Run("a server error", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(s.Close)
		c := &OSVClient{Endpoint: s.URL}
		_, _, err := c.Ask([]Identity{{DB: "npm", Name: "a"}})
		if err == nil || !strings.Contains(err.Error(), "429") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a body that is not JSON", func(t *testing.T) {
		s, _ := osvServer(t, `not json`)
		c := &OSVClient{Endpoint: s.URL}
		if _, _, err := c.Ask([]Identity{{DB: "npm", Name: "a"}}); err == nil {
			t.Error("a non-JSON body was accepted")
		}
	})
	t.Run("nothing to ask", func(t *testing.T) {
		// No request at all, and no error: a pantry whose identities are all
		// repository-keyed is a real state.
		c := &OSVClient{Endpoint: "http://127.0.0.1:1/"}
		found, skipped, err := c.Ask([]Identity{{DB: "GitHub", Name: "o/r"}})
		if err != nil || len(found) != 0 || len(skipped) != 1 {
			t.Errorf("found = %v, skipped = %v, err = %v", found, skipped, err)
		}
	})
}

// The default endpoint is OSV's, so a caller that sets nothing still asks the
// right server — checked through url() rather than by leaving Endpoint empty
// and letting the suite talk to the internet.
func TestTheDefaultEndpointIsOSVs(t *testing.T) {
	if got := (&OSVClient{}).url(); got != OSVEndpoint {
		t.Errorf("url() = %q, want %q", got, OSVEndpoint)
	}
	if got := (&OSVClient{Endpoint: "http://x/"}).url(); got != "http://x/" {
		t.Errorf("url() = %q, want the configured one", got)
	}
	if !strings.HasPrefix(OSVEndpoint, "https://api.osv.dev/") || !strings.HasSuffix(OSVEndpoint, "/querybatch") {
		t.Errorf("OSVEndpoint = %q", OSVEndpoint)
	}
}

// A body that stops short is not an empty answer. The server promises 100
// bytes and sends 1, so the read fails after the status check passed — the one
// failure that would otherwise look like "OSV knows nothing".
func TestAskReportsATruncatedBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "{")
	}))
	t.Cleanup(s.Close)
	c := &OSVClient{Endpoint: s.URL}
	_, _, err := c.Ask([]Identity{{DB: "npm", Name: "a"}})
	if err == nil {
		t.Fatal("a truncated body was read as no findings")
	}
	if !strings.Contains(err.Error(), "vuln: osv:") {
		t.Errorf("err = %v", err)
	}
}

func TestMergeDetectionsAddsOnlyWhatIsNew(t *testing.T) {
	f := mustParse(t, `schema-version: 1
project: crates.io/bat
advisories:
  - id: RUSTSEC-2021-0106
    aliases: [CVE-2021-36753]
    events:
      - timestamp: 2024-01-01T00:00:00Z
        type: false-positive-determination
        justification: vulnerable_code_not_present
`)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	n := MergeDetections(f, []Finding{
		{Identity: Identity{DB: "crates.io", Name: "bat"}, ID: "RUSTSEC-2021-0106"},
		// ⛔ The SAME vulnerability under its CVE. OSV's batch answer carries
		// no aliases, so the only defence against a double record is checking
		// the aliases we already hold.
		{Identity: Identity{DB: "crates.io", Name: "bat"}, ID: "CVE-2021-36753"},
		{Identity: Identity{DB: "crates.io", Name: "bat"}, ID: "GHSA-new-one"},
	}, now)
	if n != 1 {
		t.Fatalf("added %d, want only the new one", n)
	}
	if len(f.Advisories) != 2 {
		t.Fatalf("advisories = %d", len(f.Advisories))
	}
	// ⛔ THE DECISION IS UNTOUCHED. A sweep that reset a
	// false-positive-determination to a detection would make the triage never
	// converge: every run would undo the last one.
	if f.Advisories[0].Latest().Type != EventFalsePositive {
		t.Errorf("the existing decision was overwritten: %+v", f.Advisories[0])
	}
	added := f.Advisories[1]
	if added.ID != "GHSA-new-one" || added.Latest().Type != EventDetection {
		t.Errorf("added = %+v", added)
	}
	if !added.Events[0].Timestamp.Equal(now) {
		t.Errorf("timestamp = %v, want %v", added.Events[0].Timestamp, now)
	}
	// WHICH KEY found it.
	if !strings.Contains(added.Events[0].Note, "crates.io:bat") {
		t.Errorf("note = %q, want the identity that found it", added.Events[0].Note)
	}
}

// The same id twice in ONE sweep is recorded once. Two identities of the same
// project can both return it.
func TestMergeDetectionsIsIdempotentWithinOneSweep(t *testing.T) {
	f := &AdvisoryFile{SchemaVersion: AdvisoryVersion, Project: "p"}
	n := MergeDetections(f, []Finding{
		{Identity: Identity{DB: "npm", Name: "a"}, ID: "GHSA-x"},
		{Identity: Identity{DB: "crates.io", Name: "a"}, ID: "GHSA-x"},
	}, time.Now())
	if n != 1 || len(f.Advisories) != 1 {
		t.Errorf("added %d, advisories %d", n, len(f.Advisories))
	}
}

// What it writes must be readable by the parser, or the sweep produces files
// `bk advisories` refuses.
func TestWhatMergeWritesParsesBack(t *testing.T) {
	f := &AdvisoryFile{SchemaVersion: AdvisoryVersion, Project: "crates.io/bat"}
	MergeDetections(f, []Finding{{Identity: Identity{DB: "crates.io", Name: "bat"}, ID: "GHSA-x"}}, time.Now())
	b, err := yamlMarshal(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAdvisories(b)
	if err != nil {
		t.Fatalf("what the sweep writes does not parse back: %v\n%s", err, b)
	}
	if got.Project != "crates.io/bat" || len(got.Advisories) != 1 {
		t.Errorf("round trip = %+v", got)
	}
}

// yamlMarshal is the same encoder the sweep uses, named here so the round-trip
// test above cannot pass by using a different one.
func yamlMarshal(v any) ([]byte, error) { return yaml.Marshal(v) }

// ⛔ THE VERSION IS WHAT MAKES THE ANSWER ACTIONABLE. Asking by identity alone
// returns every advisory a package has ever carried: measured over the pkgxdev
// pantry on 2026-10-17, 748 findings across 21 projects, most of them fixed
// long before the version we publish. Asking at a version returns the ones a
// person has to decide about.
func TestAskPinnedSendsTheVersionWeShip(t *testing.T) {
	s, asked := osvServer(t, `{"results":[{}]}`)
	c := &OSVClient{Endpoint: s.URL}
	_, _, err := c.AskPinned([]Pinned{
		{Identity: Identity{DB: "npm", Name: "next"}, Version: "14.2.3"},
	})
	if err != nil {
		t.Fatalf("AskPinned: %v", err)
	}
	if !strings.Contains(*asked, `"version":"14.2.3"`) {
		t.Errorf("the request does not carry the version:\n%s", *asked)
	}
	if !strings.Contains(*asked, `"name":"next"`) {
		t.Errorf("the request lost the package:\n%s", *asked)
	}
}

// ⛔ AND THE OTHER DIRECTION, which is the half that can rot silently. Ask
// must send NO version field at all: `"version":""` is not "any version", it
// is a version named "", and OSV would answer about nothing. omitempty is
// what makes the two calls different questions, and a test that only checked
// AskPinned would pass with omitempty deleted.
func TestAskSendsNoVersionField(t *testing.T) {
	s, asked := osvServer(t, `{"results":[{}]}`)
	c := &OSVClient{Endpoint: s.URL}
	if _, _, err := c.Ask([]Identity{{DB: "npm", Name: "next"}}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if strings.Contains(*asked, `"version"`) {
		t.Errorf("Ask sent a version field, so it no longer asks about the package as a whole:\n%s", *asked)
	}
}

// An unanswerable identity is dropped before the request here too, and the
// version does not rescue it: a repository key is unanswerable whatever
// version is attached.
func TestAskPinnedStillDropsWhatOSVCannotAnswer(t *testing.T) {
	s, asked := osvServer(t, `{"results":[{}]}`)
	c := &OSVClient{Endpoint: s.URL}
	found, skipped, err := c.AskPinned([]Pinned{
		{Identity: Identity{DB: "npm", Name: "next"}, Version: "14.2.3"},
		{Identity: Identity{DB: "GitHub", Name: "curl/curl"}, Version: "8.20.0"},
	})
	if err != nil {
		t.Fatalf("AskPinned: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("found = %v", found)
	}
	if len(skipped) != 1 || skipped[0].Name != "curl/curl" {
		t.Errorf("skipped = %v, want the repository identity alone", skipped)
	}
	if strings.Contains(*asked, "curl/curl") {
		t.Errorf("an unanswerable identity was sent anyway:\n%s", *asked)
	}
}
