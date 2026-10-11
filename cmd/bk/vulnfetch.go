package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-pkgx/bk/recipefile"
	"github.com/go-pkgx/bk/vuln"
	"gopkg.in/yaml.v3"
)

// runVulnFetch asks OSV what it knows about a pantry, and (only with -write)
// records what comes back as `detection` events.
//
// It REPORTS by default. A sweep that writes on its first run writes whatever
// its first run happened to find, into files a person then has to audit; and
// the thing worth knowing before writing anything is the scale — whether 129
// queryable projects yield ten findings or four hundred.
//
// This is the only command in bk that talks to a vulnerability database, and
// it is one HTTP request per thousand identities. Nothing here touches the
// GitHub API, so nothing here competes with Renovate.
func runVulnFetch(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("vulnfetch", flag.ContinueOnError)
	fset.SetOutput(stderr)
	dir := fset.String("dir", ".", "the pantry checkout to read")
	table := fset.String("table", "", "an identity table declaring what the recipes do not say")
	out := fset.String("advisories", "advisories", "the advisory tree to record detections in")
	write := fset.Bool("write", false, "write detections into the advisory tree instead of only reporting")
	endpoint := fset.String("endpoint", "", "OSV batch query endpoint (default "+vuln.OSVEndpoint+")")
	only := fset.String("project", "", "ask about this project alone")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	tab, code := loadIdentityTable(*table, stderr)
	if code != 0 {
		return code
	}

	ids, byIdentity, code := pantryIdentities(*dir, *only, tab, stdout, stderr)
	if code != 0 {
		return code
	}

	client := &vuln.OSVClient{Endpoint: *endpoint}
	found, skipped, err := client.Ask(ids)
	if err != nil {
		fmt.Fprintf(stderr, "vulnfetch: %v\n", err)
		return 1
	}

	// ⛔ WHAT WAS NOT ASKED, FIRST. OSV cannot answer about a repository, and
	// an unasked question looks exactly like a clean answer. Measured: 1186 of
	// this pantry's identities are repository-keyed, so a sweep that stayed
	// quiet about them would report "no known vulnerabilities" for three
	// quarters of the fleet.
	fmt.Fprintf(stdout, "%d identity(ies) asked, %d not answerable by OSV (repository- or CPE-keyed)\n",
		len(ids)-len(skipped), len(skipped))

	perProject := map[string][]vuln.Finding{}
	for _, f := range found {
		for _, project := range byIdentity[f.Identity] {
			perProject[project] = append(perProject[project], f)
		}
	}
	fmt.Fprintf(stdout, "%d finding(s) across %d project(s)\n", len(found), len(perProject))
	for _, project := range sortedProjects(perProject) {
		fmt.Fprintf(stdout, "  %s\n", project)
		for _, f := range perProject[project] {
			fmt.Fprintf(stdout, "    %-24s %s:%s\n", f.ID, f.Identity.DB, f.Identity.Name)
		}
	}
	if !*write {
		if len(found) > 0 {
			fmt.Fprintln(stdout, "nothing written — pass -write to record these as detections")
		}
		return 0
	}
	return recordDetections(*out, perProject, stdout, stderr)
}

// loadIdentityTable reads the optional table, or returns the exit code.
func loadIdentityTable(path string, stderr io.Writer) (*vuln.Table, int) {
	if path == "" {
		return nil, 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "vulnfetch: %v\n", err)
		return nil, 1
	}
	tab, err := vuln.ParseTable(b)
	if err != nil {
		fmt.Fprintf(stderr, "vulnfetch: %v\n", err)
		return nil, 1
	}
	return tab, 0
}

// pantryIdentities collects every identity in a pantry, and the projects each
// one belongs to.
//
// An identity can belong to SEVERAL projects — two recipes naming the same
// upstream repository — so the reverse map is one-to-many. Keeping it
// one-to-one would attribute a finding to whichever project was read last.
func pantryIdentities(dir, only string, tab *vuln.Table, stdout, stderr io.Writer) ([]vuln.Identity, map[vuln.Identity][]string, int) {
	root := filepath.Join(dir, "projects")
	projects, err := lintProjects(root)
	if err != nil {
		fmt.Fprintf(stderr, "vulnfetch: %v\n", err)
		return nil, nil, 1
	}
	if len(projects) == 0 {
		fmt.Fprintf(stderr, "vulnfetch: no recipe under %s — the layout changed\n", root)
		return nil, nil, 1
	}
	byIdentity := map[vuln.Identity][]string{}
	var ids []vuln.Identity
	seen := map[vuln.Identity]bool{}
	read := 0
	for _, project := range projects {
		if only != "" && project != only {
			continue
		}
		r, err := recipefile.Load(dir, project)
		if err != nil {
			// Named, not counted as having no vulnerabilities.
			fmt.Fprintf(stderr, "vulnfetch: %s: %v\n", project, err)
			continue
		}
		read++
		got, _ := tab.For(project, r.Versions)
		for _, id := range got {
			byIdentity[id] = append(byIdentity[id], project)
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if read == 0 {
		fmt.Fprintf(stderr, "vulnfetch: no recipe read — %q matched nothing\n", only)
		return nil, nil, 1
	}
	fmt.Fprintf(stdout, "%d recipe(s) read from %s\n", read, root)
	return ids, byIdentity, 0
}

// recordDetections merges the findings into each project's advisory file.
func recordDetections(out string, perProject map[string][]vuln.Finding, stdout, stderr io.Writer) int {
	now := time.Now().UTC().Truncate(time.Second)
	added, touched := 0, 0
	for _, project := range sortedProjects(perProject) {
		path := filepath.Join(out, filepath.FromSlash(project), advisoryFile)
		f, err := loadOrCreate(path, project)
		if err != nil {
			fmt.Fprintf(stderr, "vulnfetch: %s: %v\n", project, err)
			return 1
		}
		n := vuln.MergeDetections(f, perProject[project], now)
		if n == 0 {
			continue
		}
		// An AdvisoryFile holds strings, ints and time.Time, all of which
		// yaml.Marshal encodes — there is no error arm here rather than an
		// unreachable one no test could enter.
		b, _ := yaml.Marshal(f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(stderr, "vulnfetch: %v\n", err)
			return 1
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			fmt.Fprintf(stderr, "vulnfetch: %v\n", err)
			return 1
		}
		added += n
		touched++
	}
	fmt.Fprintf(stdout, "%d detection(s) written across %d file(s)\n", added, touched)
	return 0
}

// loadOrCreate reads a project's advisory file, or returns an empty one.
//
// An existing file is PARSED, not appended to blindly: a file that does not
// parse must stop the sweep rather than be replaced by one holding only
// today's detections, which would delete every decision recorded in it.
func loadOrCreate(path, project string) (*vuln.AdvisoryFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &vuln.AdvisoryFile{SchemaVersion: vuln.AdvisoryVersion, Project: project}, nil
		}
		return nil, err
	}
	f, err := vuln.ParseAdvisories(b)
	if err != nil {
		return nil, err
	}
	if f.Project != project {
		return nil, fmt.Errorf("%s says project %q", path, f.Project)
	}
	return f, nil
}

func sortedProjects(m map[string][]vuln.Finding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
