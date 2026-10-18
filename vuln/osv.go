package vuln

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// Asking OSV what it knows about a set of identities.
//
// ⛔ OSV CANNOT BE ASKED ABOUT A REPOSITORY. Measured 2026-10-11 against the
// live API:
//
//	{"package":{"name":"time","ecosystem":"crates.io"}}  → 4 vulns
//	{"package":{"name":"bpb","ecosystem":"crates.io"}}   → {}
//	{"package":{"purl":"pkg:github/curl/curl"}}          → {}
//
// The first two are the two normal answers — findings, and "nothing known",
// which is not an error. The third is the one that shapes this file: an OSV
// query takes a package in an ECOSYSTEM (or a commit), and a GitHub repository
// is neither. So of the 1241 projects whose identity is derivable, only the
// ~129 in an ecosystem can be swept here; the 1186 identified by repository
// need GitHub's own advisory API, which costs the budget this fleet shares
// with Renovate. That is a different piece of work and it is not pretended to
// here.
//
// The sweep is ONE request. OSV's /v1/querybatch takes a list and answers in
// order, so 129 questions cost one round trip and no API key — nothing to do
// with the GitHub budget at all.

// OSVEndpoint is the batch query endpoint.
const OSVEndpoint = "https://api.osv.dev/v1/querybatch"

// OSVBatchLimit is how many queries go in one request. OSV documents 1000 as
// the maximum; the sweep chunks rather than assuming a pantry stays small.
const OSVBatchLimit = 1000

// Queryable reports whether an identity is one OSV can answer, and is the
// honest filter in front of a sweep: asking about the other 1186 would send
// 1186 questions and get 1186 empty answers, which reads exactly like "nothing
// is wrong".
func Queryable(id Identity) bool {
	switch id.DB {
	case "cpe", "GitHub", "GitLab":
		return false
	}
	return knownDBs[id.DB]
}

// osvRequest and osvResponse mirror the API's shapes. Only the fields used are
// decoded: the batch response carries ids and modification times, not details,
// which is all a detection needs.
type osvRequest struct {
	Queries []osvQuery `json:"queries"`
}

type osvQuery struct {
	Package osvPackage `json:"package"`
	// omitempty is what makes the two questions different ones. With a
	// version OSV returns only the advisories that AFFECT it; without the
	// field it returns everything the package has ever had. Sending an empty
	// string is not the second question — it is a version named "".
	Version string `json:"version,omitempty"`
}

type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}

type osvResponse struct {
	Results []struct {
		Vulns []struct {
			ID       string `json:"id"`
			Modified string `json:"modified"`
		} `json:"vulns"`
	} `json:"results"`
}

// OSVClient asks OSV. The HTTP client and endpoint are fields so a test can
// answer without a network, and so the fleet's own transport (which trusts the
// embedded CA bundle, for a FROM-scratch image with no system trust store) can
// be handed in.
type OSVClient struct {
	HTTP     *http.Client
	Endpoint string
}

// Finding is one vulnerability id OSV returned for one identity.
//
// It keeps the IDENTITY, not just the id, because "which key produced this
// hit" is the first thing a person triaging it needs — and with two keys per
// project it is not recoverable afterwards.
type Finding struct {
	Identity Identity
	ID       string
	Modified string
	// Version is the published version the question was asked AT, empty when
	// the question was about the package as a whole. It is what lets a
	// detection say which version it is about — see Event.DetectedVersion.
	Version string
}

// Ask returns what OSV knows about each identity, in one request per
// OSVBatchLimit identities.
//
// Identities OSV cannot answer are DROPPED BEFORE the request and reported in
// the second return value, rather than being sent and coming back empty. The
// difference matters: an unanswerable question and a clean answer look
// identical in the response, and conflating them would report 1186 repository
// identities as "no known vulnerabilities".
func (c *OSVClient) Ask(ids []Identity) (found []Finding, skipped []Identity, err error) {
	ps := make([]Pinned, len(ids))
	for i, id := range ids {
		ps[i] = Pinned{Identity: id}
	}
	return c.AskPinned(ps)
}

// Pinned is an identity AT a version — the thing we actually publish.
//
// Separate from Identity rather than a field on it: an identity says WHO a
// package is, and `versions.Identity` is shared with code that has no version
// to offer. Conflating them would make every caller answer a question it was
// not asked.
//
// An empty Version means "any version", which is Ask's behaviour and OSV's
// default.
type Pinned struct {
	Identity Identity
	Version  string
}

// AskPinned is Ask, told which version of each identity we ship.
//
// ⛔ THE TWO ANSWER DIFFERENT QUESTIONS, and the difference is the whole point.
// Measured over the pkgxdev pantry on 2026-10-17: asking by identity alone
// returned **748 findings across 21 projects** — every advisory those packages
// have ever carried, including ones fixed long before the version we publish.
// A maintainer reading 748 cannot act on it, and a list nobody can act on is
// read once and then never again.
//
// Asking at a version returns only what affects that version, which is a list
// with a decision behind each line: rebuild, or record a VEX statement saying
// why not.
func (c *OSVClient) AskPinned(ps []Pinned) (found []Finding, skipped []Identity, err error) {
	var ask []Pinned
	for _, p := range ps {
		if Queryable(p.Identity) {
			ask = append(ask, p)
		} else {
			skipped = append(skipped, p.Identity)
		}
	}
	for chunk := range slices.Chunk(ask, OSVBatchLimit) {
		f, err := c.askChunk(chunk)
		if err != nil {
			return nil, skipped, err
		}
		found = append(found, f...)
	}
	return found, skipped, nil
}

// url is the endpoint this client asks.
//
// A method rather than three lines inside askChunk, because the DEFAULT is the
// branch a test cannot otherwise enter: covering it by leaving Endpoint empty
// would send a real request to OSV from the test suite.
func (c *OSVClient) url() string {
	if c.Endpoint == "" {
		return OSVEndpoint
	}
	return c.Endpoint
}

func (c *OSVClient) askChunk(ps []Pinned) ([]Finding, error) {
	req := osvRequest{Queries: make([]osvQuery, 0, len(ps))}
	for _, p := range ps {
		req.Queries = append(req.Queries, osvQuery{
			Package: osvPackage{Name: p.Identity.Name, Ecosystem: p.Identity.DB},
			Version: p.Version,
		})
	}
	// osvRequest holds nothing but strings and a slice of them, so marshalling
	// it cannot fail — there is no error arm here rather than an unreachable
	// one that no test could enter.
	body, _ := json.Marshal(req)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Post(c.url(), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vuln: osv: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vuln: osv: %s", resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("vuln: osv: %w", err)
	}
	var out osvResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("vuln: osv: %w", err)
	}
	// ⛔ THE ANSWER IS POSITIONAL, SO ITS LENGTH IS LOAD-BEARING. OSV answers
	// in the order asked and says nothing about which query each result
	// belongs to. A response with fewer results than queries would silently
	// attribute every finding to the wrong package from that point on.
	if len(out.Results) != len(ps) {
		return nil, fmt.Errorf("vuln: osv: asked %d, answered %d — the results are positional and cannot be matched up",
			len(ps), len(out.Results))
	}
	var found []Finding
	for i, r := range out.Results {
		for _, v := range r.Vulns {
			found = append(found, Finding{Identity: ps[i].Identity, ID: v.ID, Modified: v.Modified, Version: ps[i].Version})
		}
	}
	return found, nil
}

// MergeDetections adds a `detection` event for every id not already recorded,
// and returns how many were added.
//
// ⛔ AN EXISTING ID IS LEFT ENTIRELY ALONE. A sweep must not overwrite a
// decision: a vulnerability already marked `fixed` or
// `false-positive-determination` would otherwise be dragged back to
// `under_investigation` on every run, and the triage would never converge. The
// id is also checked against every advisory's ALIASES, because a record filed
// under its CVE is the same record OSV returns under a GHSA.
// ⛔ AND A KNOWN ID IS DETECTED AGAIN WHEN THE PUBLISHED VERSION CHANGES,
// because a decision taken about one version says nothing about the next —
// go-pkgx/bk#340. The new detection carries the version, so the next reader
// can see which one was examined.
//
// Only when the record ALREADY names versions. A record written before this
// field existed names none, and treating that as "no event covers 2.0.0"
// would re-detect every one of them on the first scan — 90 findings going
// red at once, which is how a gate teaches people to ignore it. Such a
// record keeps the old id-only behaviour and grows into the new one the
// first time something is detected at a version.
func MergeDetections(f *AdvisoryFile, found []Finding, now time.Time) int {
	known := map[string]bool{}
	// versionsSeen[id] is the set of published versions already recorded for
	// it, and scoped[id] says whether the record names any version at all.
	versionsSeen := map[string]map[string]bool{}
	scoped := map[string]bool{}
	note := func(id string, a Advisory) {
		known[id] = true
		if versionsSeen[id] == nil {
			versionsSeen[id] = map[string]bool{}
		}
		for _, e := range a.Events {
			if e.DetectedVersion != "" {
				versionsSeen[id][e.DetectedVersion] = true
				scoped[id] = true
			}
		}
	}
	for _, a := range f.Advisories {
		note(a.ID, a)
		for _, al := range a.Aliases {
			note(al, a)
		}
	}
	added := 0
	for _, fi := range found {
		switch {
		case !known[fi.ID]:
			// New id: recorded as before, now carrying its version.
		case fi.Version == "" || !scoped[fi.ID] || versionsSeen[fi.ID][fi.Version]:
			// Known, and either the question had no version, or the record
			// predates versions, or this exact version is already covered.
			continue
		}
		known[fi.ID] = true
		if versionsSeen[fi.ID] == nil {
			versionsSeen[fi.ID] = map[string]bool{}
		}
		if fi.Version != "" {
			versionsSeen[fi.ID][fi.Version] = true
			scoped[fi.ID] = true
		}
		ev := Event{
			Timestamp: now.UTC(),
			Type:      EventDetection,
			// WHICH KEY produced the hit. With two identities per project
			// this is not recoverable afterwards, and it is the first
			// thing a person triaging the finding needs.
			Note:            "reported by OSV for " + fi.Identity.DB + ":" + fi.Identity.Name,
			DetectedVersion: fi.Version,
		}
		// ⛔ A SECOND EVENT ON THE EXISTING RECORD, never a second record.
		// One advisory per id is what every reader assumes — the OpenVEX
		// emitter walks the list and would produce two statements for one
		// vulnerability. A re-detection is a new thing that HAPPENED to a
		// known vulnerability, which is exactly what an event list is for.
		if i := indexOfAdvisory(f.Advisories, fi.ID); i >= 0 {
			f.Advisories[i].Events = append(f.Advisories[i].Events, ev)
		} else {
			f.Advisories = append(f.Advisories, Advisory{ID: fi.ID, Events: []Event{ev}})
		}
		added++
	}
	return added
}

// indexOfAdvisory finds a record by its id OR by one of its aliases, because
// OSV can return the same vulnerability under a different name than the one
// it was first filed under — which is why MergeDetections indexes aliases
// too. Looking it up by id alone would file the re-detection as a new record
// under the alias, and one vulnerability would have two.
func indexOfAdvisory(as []Advisory, id string) int {
	for i, a := range as {
		if a.ID == id {
			return i
		}
		for _, al := range a.Aliases {
			if al == id {
				return i
			}
		}
	}
	return -1
}
