package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/go-pkgx/bottle"
)

// unresolvedOwnerRE reads the project out of a store-relative path: everything
// before the version directory. A project name has slashes in it —
// gnupg.org/gpgme — so that directory is the only reliable terminator.
var unresolvedOwnerRE = regexp.MustCompile(`^(.+?)/v[0-9][^/]*/`)

// Seams: the two things here that need a network.
var (
	unresolvedInstall           = bottle.CompleteClosure
	unresolvedTempDir           = os.MkdirTemp
	unresolvedStdin   io.Reader = os.Stdin
)

// runUnresolved implements `bk unresolved`: which @rpath references in an
// installed closure name a file the store does not have?
//
// It is the SYMPTOM whose cause `bk undeclared` reports, and the two answer
// different questions. A missing declaration is a packaging defect; an
// unresolvable reference is what the loader will actually refuse, and a bottle
// can have one without the other — a reference to a version directory that
// moved on is declared perfectly and still resolves nowhere.
//
// Each project is installed into its OWN store. A shared one rescues
// references by accident: gpgme's undeclared libgpg-error resolved for months
// because something else in the same closure happened to bring it, and the
// failure only appeared in a closure that had gpgme and not the rescuer.
//
// It reads the Mach-O itself rather than shelling out to otool, which is an
// Xcode tool and absent from the FROM-scratch image this factory builds in.
// This command exists BECAUSE a lab binary answering the same question turned
// out to be two days old with its source lost — a tool built before the fix it
// measures gives verdicts that are plausible and wrong.
func runUnresolved(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("unresolved", flag.ContinueOnError)
	fset.SetOutput(stderr)
	store := fset.String("store", "", "install into this directory (default: a temporary one, removed after)")
	quiet := fset.Bool("quiet", false, "print only the projects that have unresolvable references")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	projects, err := undeclaredProjects(fset.Args(), unresolvedStdin)
	if err != nil {
		fmt.Fprintln(stderr, "unresolved:", err)
		return 1
	}
	if len(projects) == 0 {
		fmt.Fprintln(stderr, "unresolved: no projects (name them, or pipe them in)")
		return 2
	}

	total := 0
	for _, p := range projects {
		dir := *store
		if dir == "" {
			d, err := unresolvedTempDir("", "bk-unresolved-")
			if err != nil {
				fmt.Fprintln(stderr, "unresolved:", err)
				return 1
			}
			dir = d
			defer os.RemoveAll(dir)
		}
		if _, err := unresolvedInstall(map[string]string{p: "*"}, dir); err != nil {
			fmt.Fprintf(stderr, "unresolved: %s: %v\n", p, err)
			continue
		}
		byOwner := unresolvedByOwner(dir)
		n := 0
		for _, owner := range sortedOwners2(byOwner) {
			refs := byOwner[owner]
			if len(refs) == 0 {
				continue
			}
			n += len(refs)
			fmt.Fprintf(stdout, "%-34s %-34s %d unresolvable\n", p, owner, len(refs))
			if !*quiet {
				for _, r := range refs {
					fmt.Fprintf(stdout, "    %s\n", r)
				}
			}
		}
		if n == 0 && !*quiet {
			fmt.Fprintf(stdout, "%-34s ok\n", p)
		}
		total += n
	}
	fmt.Fprintf(stdout, "\n%d unresolvable reference(s)\n", total)
	if total > 0 {
		return 1
	}
	return 0
}

// unresolvedByOwner maps each project in the store onto the references its own
// Mach-O files name and the store cannot answer.
//
// On darwin "is it satisfied" is a STAT, not a search: a Mach-O reference is a
// path, and either the store has that file or the program does not start.
func unresolvedByOwner(dir string) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(p, dir), string(filepath.Separator)))
		m := unresolvedOwnerRE.FindStringSubmatch(rel)
		if m == nil {
			return nil
		}
		owner := m[1]
		refs, rerr := bottle.MachoNeeded(p)
		if rerr != nil {
			return nil
		}
		if _, ok := out[owner]; !ok {
			out[owner] = nil // the project was examined, even if it is clean
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref, "@rpath/") {
				continue // /usr/lib and the system frameworks are the host's
			}
			if _, serr := os.Stat(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(ref, "@rpath/")))); serr == nil {
				continue
			}
			// One line per (file, reference): the same reference from forty
			// binaries is forty occurrences of ONE defect, and printing all of
			// them buries the count that matters.
			key := owner + "\x00" + ref
			if seen[key] {
				continue
			}
			seen[key] = true
			out[owner] = append(out[owner], ref)
		}
		return nil
	})
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func sortedOwners2(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
