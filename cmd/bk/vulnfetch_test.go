package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// vulnfetch runs the command, and NEVER lets it write outside a temporary
// directory.
//
// ⛔ A TEST'S BLAST RADIUS MUST NOT DEPEND ON A FLAG DEFAULT. `-advisories`
// defaults to the relative path "advisories", and `go test` runs with the
// package directory as its working directory — so a run with -write and no
// -advisories writes into cmd/bk/advisories, inside the repository, one
// `git add -A` from being committed. That is exactly what happened: the
// mutation that flipped -write's default to true was caught by the tests, and
// left `cmd/bk/advisories/crates.io/bat/advisories.yaml` behind, which then got
// staged.
//
// So the helper supplies -advisories unless the caller already did. The guard
// belongs here rather than in each test, because the next test written without
// it is the one that escapes.
func vulnfetch(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	if !slices.Contains(args, "-advisories") {
		args = append(args, "-advisories", filepath.Join(t.TempDir(), "advisories"))
	}
	var out, errb bytes.Buffer
	rc := runVulnFetch(args, &out, &errb)
	return rc, out.String(), errb.String()
}

// Nothing the suite runs may write into the working directory, whatever the
// flag defaults are. A positive control: the guard is asserted to be able to
// fail, by checking the helper really substitutes a temporary path.
func TestTheSuiteCannotWriteIntoTheRepository(t *testing.T) {
	if _, err := os.Stat("advisories"); err == nil {
		t.Fatal("cmd/bk/advisories exists — a test wrote into the repository")
	}
	url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
	// -write with no -advisories: the helper must redirect it.
	if rc, _, errb := vulnfetch(t, "-dir", fetchPantry(t), "-endpoint", url, "-write"); rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, errb)
	}
	if _, err := os.Stat("advisories"); err == nil {
		t.Error("the helper did not redirect the default advisory tree")
	}
}

// fetchServer answers one batch query with the given body and records it.
func fetchServer(t *testing.T, body string) (string, *string) {
	t.Helper()
	var asked string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		asked = string(b)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s.URL, &asked
}

// A pantry with one queryable project and one that is not.
func fetchPantry(t *testing.T) string {
	return lintTree(t, map[string]string{
		"crates.io/bat/package.yml": "distributable:\n  url: http://x/y.tgz\nversions:\n  github: sharkdp/bat\n",
		"curl.se/package.yml":       "distributable:\n  url: http://x/y.tgz\nversions:\n  github: curl/curl\n",
	})
}

func TestVulnFetchReportsFindings(t *testing.T) {
	// crates.io:bat is asked; GitHub:sharkdp/bat and GitHub:curl/curl are not.
	url, asked := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106","modified":"2026-02-04T02:31:56Z"}]}]}`)
	rc, out, errb := vulnfetch(t, "-dir", fetchPantry(t), "-endpoint", url)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr:\n%s", rc, errb)
	}
	for _, want := range []string{
		"2 recipe(s) read",
		"1 identity(ies) asked, 2 not answerable by OSV",
		"1 finding(s) across 1 project(s)",
		"crates.io/bat",
		"RUSTSEC-2021-0106",
		"nothing written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	// ⛔ The repository identities were not SENT. An unanswerable question and
	// a clean answer look identical in the response.
	if strings.Contains(*asked, "curl/curl") || strings.Contains(*asked, "sharkdp") {
		t.Errorf("a repository identity was sent: %s", *asked)
	}
}

// ⛔ WHAT WAS NOT ASKED, SAID OUT LOUD. 1207 of this fleet's identities are
// repository- or CPE-keyed; a sweep that stayed quiet about them would read as
// "no known vulnerabilities" for three quarters of the pantry.
func TestVulnFetchSaysWhatItCouldNotAsk(t *testing.T) {
	url, _ := fetchServer(t, `{"results":[{}]}`)
	_, out, _ := vulnfetch(t, "-dir", fetchPantry(t), "-endpoint", url)
	if !strings.Contains(out, "2 not answerable by OSV (repository- or CPE-keyed)") {
		t.Errorf("want the unasked count:\n%s", out)
	}
	if !strings.Contains(out, "0 finding(s)") {
		t.Errorf("want the zero stated, not implied:\n%s", out)
	}
}

func TestVulnFetchWritesDetections(t *testing.T) {
	url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
	outDir := filepath.Join(t.TempDir(), "advisories")
	rc, out, errb := vulnfetch(t, "-dir", fetchPantry(t), "-endpoint", url, "-advisories", outDir, "-write")
	if rc != 0 {
		t.Fatalf("rc = %d, stderr:\n%s", rc, errb)
	}
	if !strings.Contains(out, "1 detection(s) written across 1 file(s)") {
		t.Errorf("want the written count:\n%s", out)
	}
	// The nested project name is a PATH, mirroring the pantry.
	b, err := os.ReadFile(filepath.Join(outDir, "crates.io", "bat", advisoryFile))
	if err != nil {
		t.Fatalf("nothing written: %v", err)
	}
	for _, want := range []string{"project: crates.io/bat", "RUSTSEC-2021-0106", "type: detection", "crates.io:bat"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("want %q in:\n%s", want, b)
		}
	}
	// ⛔ And what it wrote must be readable by `bk advisories`, or the sweep
	// produces files the validator refuses.
	rc, _, errb = advisories(t, "-dir", outDir)
	if rc != 0 {
		t.Errorf("bk advisories refuses what the sweep wrote: %s", errb)
	}
}

// A second run adds nothing and does not touch the file.
func TestVulnFetchIsIdempotent(t *testing.T) {
	url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
	outDir := filepath.Join(t.TempDir(), "advisories")
	pantry := fetchPantry(t)
	if rc, _, e := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write"); rc != 0 {
		t.Fatalf("first run: %s", e)
	}
	path := filepath.Join(outDir, "crates.io", "bat", advisoryFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rc, out, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write")
	if rc != 0 {
		t.Fatalf("second run: %s", errb)
	}
	if !strings.Contains(out, "0 detection(s) written across 0 file(s)") {
		t.Errorf("want nothing added:\n%s", out)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the file was rewritten:\n%s\n---\n%s", before, after)
	}
}

// ⛔ A DECISION ALREADY RECORDED SURVIVES THE SWEEP. Otherwise every run would
// undo the last one and the triage would never converge.
func TestVulnFetchDoesNotUndoADecision(t *testing.T) {
	url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
	outDir := t.TempDir()
	p := filepath.Join(outDir, "crates.io", "bat", advisoryFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	decided := `schema-version: 1
project: crates.io/bat
advisories:
  - id: RUSTSEC-2021-0106
    events:
      - timestamp: 2024-01-01T00:00:00Z
        type: false-positive-determination
        justification: vulnerable_code_not_present
`
	if err := os.WriteFile(p, []byte(decided), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc, _, e := vulnfetch(t, "-dir", fetchPantry(t), "-endpoint", url, "-advisories", outDir, "-write"); rc != 0 {
		t.Fatalf("rc, stderr: %s", e)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "false-positive-determination") {
		t.Errorf("the decision was overwritten:\n%s", b)
	}
	if strings.Contains(string(b), "type: detection") {
		t.Errorf("a detection was added beside the decision:\n%s", b)
	}
}

func TestVulnFetchRefusals(t *testing.T) {
	url, _ := fetchServer(t, `{"results":[{}]}`)
	pantry := fetchPantry(t)
	t.Run("an empty pantry", func(t *testing.T) {
		rc, _, errb := vulnfetch(t, "-dir", t.TempDir(), "-endpoint", url)
		if rc != 1 || !strings.Contains(errb, "the layout changed") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("a -project that matches nothing", func(t *testing.T) {
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-project", "nope.org")
		if rc != 1 || !strings.Contains(errb, "matched nothing") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("a table that is not there", func(t *testing.T) {
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-table", filepath.Join(t.TempDir(), "x.yml"))
		if rc != 1 || !strings.Contains(errb, "x.yml") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("a table that does not parse", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "t.yml")
		if err := os.WriteFile(p, []byte("schema-version: 1\nprojects:\n  p:\n    cpe: [openssl]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-table", p)
		if rc != 1 || !strings.Contains(errb, "no vendor") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("an unreachable endpoint", func(t *testing.T) {
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", "http://127.0.0.1:1/")
		if rc != 1 || !strings.Contains(errb, "vulnfetch:") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("an unknown flag", func(t *testing.T) {
		if rc, _, _ := vulnfetch(t, "-nonesuch"); rc != 2 {
			t.Error("an unknown flag must be a usage error")
		}
	})
	t.Run("an advisory file that does not parse", func(t *testing.T) {
		// ⛔ STOPS the sweep. Replacing it with one holding only today's
		// detections would delete every decision recorded in it.
		url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
		outDir := t.TempDir()
		p := filepath.Join(outDir, "crates.io", "bat", advisoryFile)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("schema-version: 9\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write")
		if rc != 1 || !strings.Contains(errb, "schema-version 9") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("an advisory file naming another project", func(t *testing.T) {
		url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
		outDir := t.TempDir()
		p := filepath.Join(outDir, "crates.io", "bat", advisoryFile)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "schema-version: 1\nproject: curl.se\nadvisories:\n  - id: X\n    events:\n      - timestamp: 2024-01-01T00:00:00Z\n        type: detection\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write")
		if rc != 1 || !strings.Contains(errb, `says project "curl.se"`) {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("a recipe that cannot be read", func(t *testing.T) {
		url, _ := fetchServer(t, `{"results":[{}]}`)
		dir := lintTree(t, map[string]string{
			"good.org/package.yml": "distributable:\n  url: http://x/y.tgz\nversions:\n  npm: good\n",
			"bad.org/package.hcl":  "build { script = \n",
		})
		// Named on stderr, and the readable one still measured: one bad recipe
		// must not void the sweep.
		rc, out, errb := vulnfetch(t, "-dir", dir, "-endpoint", url)
		if rc != 0 {
			t.Fatalf("rc = %d, stderr = %q", rc, errb)
		}
		if !strings.Contains(errb, "bad.org") {
			t.Errorf("the unreadable recipe was not named: %q", errb)
		}
		if !strings.Contains(out, "1 recipe(s) read") {
			t.Errorf("want the readable one counted:\n%s", out)
		}
	})
	t.Run("a tree whose directory cannot be created", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write to an unwritable directory")
		}
		url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
		outDir := t.TempDir()
		if err := os.Chmod(outDir, 0o500); err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { _ = os.Chmod(outDir, 0o755) })
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write")
		if rc != 1 || !strings.Contains(errb, "vulnfetch:") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("a file that cannot be written", func(t *testing.T) {
		// A DIFFERENT failure from the one above: here the directory chain
		// exists, so MkdirAll succeeds and the write is what fails. Both exit
		// 1, which is why they need separate cases — the coverage profile is
		// the only thing that tells them apart.
		if os.Geteuid() == 0 {
			t.Skip("root can write to an unwritable directory")
		}
		url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
		outDir := t.TempDir()
		leaf := filepath.Join(outDir, "crates.io", "bat")
		if err := os.MkdirAll(leaf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(leaf, 0o500); err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { _ = os.Chmod(leaf, 0o755) })
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write")
		if rc != 1 || !strings.Contains(errb, "vulnfetch:") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("an advisory file that cannot be read", func(t *testing.T) {
		// Not the absent case: a file we cannot open must stop the sweep, or
		// it would be replaced by one holding only today's detections.
		if os.Geteuid() == 0 {
			t.Skip("root can read an unreadable file")
		}
		url, _ := fetchServer(t, `{"results":[{"vulns":[{"id":"RUSTSEC-2021-0106"}]}]}`)
		outDir := t.TempDir()
		p := filepath.Join(outDir, "crates.io", "bat", advisoryFile)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("schema-version: 1\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
		rc, _, errb := vulnfetch(t, "-dir", pantry, "-endpoint", url, "-advisories", outDir, "-write")
		if rc != 1 || !strings.Contains(errb, "vulnfetch:") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
	})
	t.Run("a pantry the walk cannot descend", func(t *testing.T) {
		// An error, not an empty corpus. Both exit 1 and only the message
		// distinguishes them.
		if os.Geteuid() == 0 {
			t.Skip("root can read an unreadable directory")
		}
		dir := lintTree(t, map[string]string{"a.org/package.yml": "versions:\n  npm: a\n"})
		sub := filepath.Join(dir, "projects", "a.org")
		if err := os.Chmod(sub, 0o000); err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
		rc, out, errb := vulnfetch(t, "-dir", dir, "-endpoint", url)
		if rc != 1 || !strings.Contains(errb, "vulnfetch:") {
			t.Errorf("rc = %d, stderr = %q", rc, errb)
		}
		if strings.Contains(errb, "the layout changed") {
			t.Errorf("a walk error was reported as an empty pantry: %q", errb)
		}
		if out != "" {
			t.Errorf("no totals belong to a run that could not read the tree: %q", out)
		}
	})
}

// -project narrows the sweep to one recipe, which is how a single finding gets
// re-checked without asking about 1563.
func TestVulnFetchCanAskAboutOneProject(t *testing.T) {
	url, asked := fetchServer(t, `{"results":[{}]}`)
	rc, out, errb := vulnfetch(t, "-dir", fetchPantry(t), "-endpoint", url, "-project", "crates.io/bat")
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, errb)
	}
	if !strings.Contains(out, "1 recipe(s) read") {
		t.Errorf("want one recipe read:\n%s", out)
	}
	if strings.Contains(*asked, "curl") {
		t.Errorf("the other project was asked about: %s", *asked)
	}
}

// A declared identity is used, so a project the recipes cannot identify can
// still be swept once someone has written its name down.
func TestVulnFetchUsesTheDeclaredTable(t *testing.T) {
	url, asked := fetchServer(t, `{"results":[{}]}`)
	dir := lintTree(t, map[string]string{
		"openssl.org/package.yml": "distributable:\n  url: http://x/y.tgz\nversions:\n  url: http://openssl.org/s/\n  match: /x-\\d/\n",
	})
	tab := filepath.Join(t.TempDir(), "t.yml")
	if err := os.WriteFile(tab, []byte("schema-version: 1\nprojects:\n  openssl.org:\n    ConanCenter: [openssl]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, out, errb := vulnfetch(t, "-dir", dir, "-endpoint", url, "-table", tab)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, errb)
	}
	if !strings.Contains(out, "1 identity(ies) asked, 0 not answerable") {
		t.Errorf("the declared identity was not asked:\n%s", out)
	}
	if !strings.Contains(*asked, "ConanCenter") {
		t.Errorf("asked %s", *asked)
	}
}

// Through the top-level dispatch.
func TestVulnFetchDispatch(t *testing.T) {
	url, _ := fetchServer(t, `{"results":[{}]}`)
	var out, errb bytes.Buffer
	if code := run([]string{"vulnfetch", "-dir", fetchPantry(t), "-endpoint", url}, &out, &errb); code != 0 {
		t.Errorf("code = %d, stderr = %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "finding(s) across") {
		t.Errorf("want the totals on stdout, got %q", out.String())
	}
}
