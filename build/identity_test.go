package build

import (
	"strings"
	"testing"

	"github.com/go-zpkg/zbk/moustache"
	"github.com/go-zpkg/zbk/pantry"
	"github.com/go-zpkg/zbk/target"
)

func idRecipe() *pantry.Recipe {
	return &pantry.Recipe{
		Distributable: map[string]any{"url": "https://x/v{{version.raw}}.tar.gz"},
		Build:         map[string]any{"script": []any{"make install"}},
	}
}

var idTarget = target.Target{Platform: "linux", Arch: "x86-64"}

func mustIdentity(t *testing.T, rec *pantry.Recipe, project, version string, tgt target.Target, deps []moustache.Dep) string {
	t.Helper()
	got, err := identity(rec, project, version, tgt, deps)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if got == "" {
		t.Fatal("identity is empty")
	}
	return got
}

// ⛔⛔ THE SAME INPUTS GIVE THE SAME ANSWER, TWICE AND IN ANY ORDER. This is
// the one property the whole hashed layout rests on: the hash goes in the
// install prefix, which is baked into every binary the build links, so an
// identity that varied between two runs of the same build would make the
// store unable to say that a rebuild produced the same thing.
func TestAnIdentityIsStableAcrossRunsAndDependencyOrder(t *testing.T) {
	rec := idRecipe()
	a := mustIdentity(t, rec, "acme.org/tool", "1.2.3", idTarget, nil)
	if b := mustIdentity(t, rec, "acme.org/tool", "1.2.3", idTarget, nil); a != b {
		t.Errorf("two runs disagree: %q vs %q", a, b)
	}

	// Dependencies reach this as a slice built by walking a map, so the order
	// is whatever Go felt like. Sorting inside identity() is what makes the
	// two spellings below one identity, and the assertion is that they are.
	deps := []moustache.Dep{
		{Project: "openssl.org", Path: "/store/openssl.org/v3.5.8-habcdefghijklmnop"},
		{Project: "zlib.net", Path: "/store/zlib.net/v1.3.2-hqrstuvwxyz234567"},
	}
	fwd := mustIdentity(t, rec, "acme.org/tool", "1.2.3", idTarget, deps)
	rev := mustIdentity(t, rec, "acme.org/tool", "1.2.3", idTarget,
		[]moustache.Dep{deps[1], deps[0]})
	if fwd != rev {
		t.Errorf("the order a map was walked in changed the identity: %q vs %q", fwd, rev)
	}
	if fwd == a {
		t.Error("adding two dependencies did not change the identity")
	}
}

// ⛔⛔ EVERY INPUT IS LOAD-BEARING, AND EACH IS CHECKED SEPARATELY. A table
// that only asserted "these all differ from the base" would pass with an
// input silently dropped from the hash, as long as one other input varied
// with it; here each case changes exactly one thing.
func TestEveryInputChangesTheIdentity(t *testing.T) {
	base := mustIdentity(t, idRecipe(), "acme.org/tool", "1.2.3", idTarget, nil)

	otherArch := idRecipe()
	otherScript := idRecipe()
	otherScript.Build = map[string]any{"script": []any{"make install", "make check"}}
	otherURL := idRecipe()
	otherURL.Distributable = map[string]any{"url": "https://y/v{{version.raw}}.tar.gz"}

	seen := map[string]string{base: "the base"}
	for _, tc := range []struct {
		what    string
		rec     *pantry.Recipe
		project string
		version string
		tgt     target.Target
		deps    []moustache.Dep
	}{
		{"the platform", otherArch, "acme.org/tool", "1.2.3",
			target.Target{Platform: "darwin", Arch: "x86-64"}, nil},
		{"the architecture", otherArch, "acme.org/tool", "1.2.3",
			target.Target{Platform: "linux", Arch: "aarch64"}, nil},
		{"the project", idRecipe(), "acme.org/other", "1.2.3", idTarget, nil},
		{"the version", idRecipe(), "acme.org/tool", "1.2.4", idTarget, nil},
		{"the build script", otherScript, "acme.org/tool", "1.2.3", idTarget, nil},
		{"the distributable", otherURL, "acme.org/tool", "1.2.3", idTarget, nil},
		{"a dependency's resolved prefix", idRecipe(), "acme.org/tool", "1.2.3", idTarget,
			[]moustache.Dep{{Project: "zlib.net", Path: "/store/zlib.net/v1.3.2-habcdefghijklmnop"}}},
	} {
		got := mustIdentity(t, tc.rec, tc.project, tc.version, tc.tgt, tc.deps)
		// Collision-checked against every earlier case, not only the base:
		// two inputs that mapped to one identity would mean the store could
		// not hold both builds, and comparing to the base alone cannot see it.
		if prev, dup := seen[got]; dup {
			t.Errorf("changing %s gives the same identity as %s: %q", tc.what, prev, got)
			continue
		}
		seen[got] = "changing " + tc.what
	}
}

// ⛔⛔ A BUILD WHOSE IDENTITY CANNOT BE COMPUTED DOES NOT BUILD. There is no
// weaker prefix to fall back to: `v<version>` is the shape two builds of one
// version overwrote each other in, and an identity that failed quietly would
// either produce it or produce a prefix hashed over less than the closure
// read. So the failure is returned and the build stops, naming the step.
func TestABuildStopsWhenItsIdentityCannotBeComputed(t *testing.T) {
	tenv(t)
	tgt := target.Target{Platform: "linux", Arch: "x86-64"}
	rec := okRecipe()
	// A channel has no JSON form, so the recipe does not serialise and
	// `spec.Hash` refuses rather than hashing over the halves it managed.
	rec.Build = map[string]any{"script": []any{"make install"}, "unserialisable": make(chan int)}

	_, err := okRunner("acme.org/tool", tgt).Build(rec, "acme.org/tool", "*", tgt, tgt, "")
	if err == nil {
		t.Fatal("Build: nil; a build with no identity has no prefix to install into")
	}
	if !strings.Contains(err.Error(), "build identity") {
		t.Errorf("the failure does not name the step: %v", err)
	}
}

// Two builds that differ ONLY in a dependency's build — same project, same
// version, a different hash in its prefix — are different builds. This is the
// edge the Merkle property rides on, and the one that makes a rebuilt
// dependency propagate.
func TestADependencysOwnHashReachesTheIdentity(t *testing.T) {
	rec := idRecipe()
	one := mustIdentity(t, rec, "acme.org/tool", "1.2.3", idTarget,
		[]moustache.Dep{{Project: "zlib.net", Path: "/store/zlib.net/v1.3.2-habcdefghijklmnop"}})
	two := mustIdentity(t, rec, "acme.org/tool", "1.2.3", idTarget,
		[]moustache.Dep{{Project: "zlib.net", Path: "/store/zlib.net/v1.3.2-hqrstuvwxyz234567"}})
	if one == two {
		t.Error("a dependency rebuilt into a different prefix left the dependent's identity alone")
	}
}
