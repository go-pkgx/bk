package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bottle"
)

// writeMachOAt builds a Mach-O by hand rather than compiling one, so these
// tests run on every lane: the coverage gate is on linux.
func writeMachOAt(t *testing.T, p string, loads ...string) {
	t.Helper()
	le := binary.LittleEndian
	var body []byte
	for _, s := range loads {
		size := 24 + len(s) + 1
		for size%8 != 0 {
			size++
		}
		cb := make([]byte, size)
		le.PutUint32(cb[0:], 0x0c) // LC_LOAD_DYLIB
		le.PutUint32(cb[4:], uint32(size))
		le.PutUint32(cb[8:], 24)
		copy(cb[24:], s)
		body = append(body, cb...)
	}
	buf := make([]byte, 32+len(body))
	le.PutUint32(buf[0:], 0xfeedfacf)
	le.PutUint32(buf[4:], 0x0100000c)
	le.PutUint32(buf[12:], 6)
	le.PutUint32(buf[16:], uint32(len(loads)))
	le.PutUint32(buf[20:], uint32(len(body)))
	copy(buf[32:], body)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf, 0o755); err != nil {
		t.Fatal(err)
	}
}

// The question is a stat, not a search: either the store has the file the
// reference names, or the program does not start.
func TestUnresolvedByOwnerIsAStat(t *testing.T) {
	dir := t.TempDir()
	writeMachOAt(t, filepath.Join(dir, "a.org/v1.0.0/lib/liba.1.dylib"))
	writeMachOAt(t, filepath.Join(dir, "top.org/v9.0/bin/tool"),
		"@rpath/a.org/v1.0.0/lib/liba.1.dylib", // present
		"@rpath/gone.org/v2/lib/libgone.2.dylib",
		"/usr/lib/libSystem.B.dylib", // the host's
	)
	got := unresolvedByOwner(dir)
	if len(got["top.org"]) != 1 || got["top.org"][0] != "@rpath/gone.org/v2/lib/libgone.2.dylib" {
		t.Errorf("top.org = %v", got["top.org"])
	}
	// A clean project is still REPORTED as examined, so "ok" means looked at
	// rather than not reached.
	if _, ok := got["a.org"]; !ok {
		t.Error("a clean project was not recorded as examined")
	}
	if len(got["a.org"]) != 0 {
		t.Errorf("a.org = %v, want clean", got["a.org"])
	}
}

// The same reference from forty binaries is forty occurrences of ONE defect.
// Printing all of them buries the count that matters.
func TestUnresolvedByOwnerCountsDistinctReferences(t *testing.T) {
	dir := t.TempDir()
	for _, b := range []string{"one", "two", "three"} {
		writeMachOAt(t, filepath.Join(dir, "x.org/v1/bin/"+b), "@rpath/gone.org/v2/lib/libgone.2.dylib")
	}
	if got := unresolvedByOwner(dir); len(got["x.org"]) != 1 {
		t.Errorf("x.org = %v, want one distinct reference", got["x.org"])
	}
}

// Each project gets its OWN store. A shared one rescues references by
// accident, which is how gpgme's undeclared libgpg-error went unnoticed.
func TestRunUnresolvedGivesEachProjectItsOwnStore(t *testing.T) {
	oldI, oldD, oldS := unresolvedInstall, unresolvedTempDir, unresolvedStdin
	defer func() { unresolvedInstall, unresolvedTempDir, unresolvedStdin = oldI, oldD, oldS }()

	var dirs []string
	base := t.TempDir()
	n := 0
	unresolvedTempDir = func(string, string) (string, error) {
		n++
		d := filepath.Join(base, "store", string(rune('a'+n)))
		dirs = append(dirs, d)
		return d, os.MkdirAll(d, 0o755)
	}
	unresolvedInstall = func(roots map[string]string, dir string) ([]bottle.Resolved, error) {
		for p := range roots {
			writeMachOAt(t, filepath.Join(dir, p, "v1/bin/tool"), "@rpath/gone.org/v2/lib/libgone.2.dylib")
		}
		return nil, nil
	}
	unresolvedStdin = strings.NewReader("")

	var out bytes.Buffer
	if rc := runUnresolved([]string{"a.org", "b.org"}, &out, &out); rc != 1 {
		t.Fatalf("rc = %d, want 1 for unresolvable references", rc)
	}
	if len(dirs) != 2 || dirs[0] == dirs[1] {
		t.Errorf("stores = %v, want one per project", dirs)
	}
	if !strings.Contains(out.String(), "2 unresolvable reference(s)") {
		t.Errorf("report:\n%s", out.String())
	}
}

// A clean store is a zero status, and says so.
func TestRunUnresolvedOnACleanStore(t *testing.T) {
	oldI, oldD := unresolvedInstall, unresolvedTempDir
	defer func() { unresolvedInstall, unresolvedTempDir = oldI, oldD }()
	base := t.TempDir()
	unresolvedTempDir = func(string, string) (string, error) { return base, nil }
	unresolvedInstall = func(roots map[string]string, dir string) ([]bottle.Resolved, error) {
		writeMachOAt(t, filepath.Join(dir, "a.org/v1/lib/liba.dylib"))
		return nil, nil
	}
	var out bytes.Buffer
	if rc := runUnresolved([]string{"a.org"}, &out, &out); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if !strings.Contains(out.String(), "a.org") || !strings.Contains(out.String(), "0 unresolvable") {
		t.Errorf("report:\n%s", out.String())
	}
}

// An install that fails is reported and the sweep continues: one unreachable
// project must not cost the answer for the rest.
func TestRunUnresolvedKeepsGoingAfterAFailedInstall(t *testing.T) {
	oldI, oldD := unresolvedInstall, unresolvedTempDir
	defer func() { unresolvedInstall, unresolvedTempDir = oldI, oldD }()
	base := t.TempDir()
	k := 0
	unresolvedTempDir = func(string, string) (string, error) {
		k++
		d := filepath.Join(base, string(rune('a'+k)))
		return d, os.MkdirAll(d, 0o755)
	}
	unresolvedInstall = func(roots map[string]string, dir string) ([]bottle.Resolved, error) {
		if roots["bad.org"] != "" {
			return nil, errors.New("no such project")
		}
		writeMachOAt(t, filepath.Join(dir, "ok.org/v1/lib/lib.dylib"))
		return nil, nil
	}
	var out, errb bytes.Buffer
	runUnresolved([]string{"bad.org", "ok.org"}, &out, &errb)
	if !strings.Contains(errb.String(), "bad.org") {
		t.Errorf("the failure was not reported: %s", errb.String())
	}
	if !strings.Contains(out.String(), "ok.org") {
		t.Errorf("the sweep stopped at the failure:\n%s", out.String())
	}
}

// Nothing to examine is a usage error, not an empty success.
func TestRunUnresolvedWithNothingToDo(t *testing.T) {
	old := unresolvedStdin
	defer func() { unresolvedStdin = old }()
	unresolvedStdin = strings.NewReader("\n# only a comment\n")
	var out bytes.Buffer
	if rc := runUnresolved(nil, &out, &out); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
}
