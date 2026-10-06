package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bottle"
)

// `bk lock` wrote locks and `bk factory` could not build from one. The
// whole point of a lock is that a later build is the SAME build: the same
// pantry commit, 2df061bd, resolved tcl to 9.0.4 and then to 9.1.0 four
// hours apart.
func TestFactoryBuildsFromALock(t *testing.T) {
	d := bottle.Lock{
		Version: bottle.LockfileVersion, Platform: "linux/x86-64",
		Generated: "2026-10-06T09:00:00Z", BK: "v0.13.0",
		Roots: []string{"curl.se"}, // ONE root
		Pins: []bottle.LockPin{ // THREE pins
			{Project: "curl.se", Version: "8.17.0", Spec: "sha256:a"},
			{Project: "openssl.org", Version: "3.6.0", Spec: "sha256:b"},
			{Project: "zlib.net", Version: "1.3.2", Spec: "sha256:c"},
		},
	}
	p := filepath.Join(t.TempDir(), "seed.lock.hcl")
	if err := os.WriteFile(p, []byte(bottle.RenderLock(d)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := factoryWant("", "", p)
	if err != nil {
		t.Fatalf("factoryWant: %v", err)
	}
	want := "curl.se@=8.17.0 openssl.org@=3.6.0 zlib.net@=1.3.2"
	if strings.Join(got, " ") != want {
		t.Errorf("want list = %v, want %s", got, want)
	}
	// EVERY PIN, not just the lock's own roots — which is the difference
	// between a lock and a hint. The fixture has one root and three pins
	// precisely so this cannot pass by coincidence.
	if len(d.Roots) != 1 || len(got) != 3 {
		t.Errorf("%d roots, %d built — the test proves nothing", len(d.Roots), len(got))
	}
	// And the words are the ones --recipes already takes, so a locked run
	// and a free one go through ONE path.
	for _, w := range got {
		if !strings.Contains(w, "@=") {
			t.Errorf("%q is not the pinned form --recipes understands", w)
		}
	}
}

// A lock is the whole set, so it wins over the other two ways of naming
// one. Taking --recipes instead would build something that is not what the
// lock says while reporting a locked build.
func TestALockBeatsRecipesAndRecipesFile(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "l.hcl")
	if err := os.WriteFile(lock, []byte(bottle.RenderLock(bottle.Lock{
		Version: bottle.LockfileVersion,
		Pins:    []bottle.LockPin{{Project: "zlib.net", Version: "1.3.2", Spec: "sha256:c"}},
	})), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "recipes.txt")
	if err := os.WriteFile(file, []byte("from.file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := factoryWant("from.flag", file, lock)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "zlib.net@=1.3.2" {
		t.Errorf("want list = %v, want the lock's pins", got)
	}

	// Without a lock, the earlier precedence is unchanged.
	if got, err := factoryWant("from.flag", file, ""); err != nil || strings.Join(got, " ") != "from.flag" {
		t.Errorf("--recipes = %v, %v", got, err)
	}
	if got, err := factoryWant("", file, ""); err != nil || strings.Join(got, " ") != "from.file" {
		t.Errorf("--recipes-file = %v, %v", got, err)
	}
}

// An unreadable or empty lock FAILS. Falling through to --recipes would
// build the wrong set under a flag that promised the right one, and
// building nothing while reporting success is how a run silently stops
// being locked.
func TestABadLockFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := factoryWant("fallback.org", "", filepath.Join(dir, "absent.hcl")); err == nil {
		t.Error("an absent lock fell through to --recipes")
	}
	empty := filepath.Join(dir, "empty.hcl")
	if err := os.WriteFile(empty, []byte("lockfile_version = 1\nlocked = {\n  \"x\" = { version = \"\", spec = \"\" }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := factoryWant("fallback.org", "", empty); err == nil {
		t.Error("a lock pinning nothing fell through to --recipes")
	} else if !strings.Contains(err.Error(), "pins nothing") {
		t.Errorf("err = %v", err)
	}
}
