package build

import (
	"strings"
	"testing"
)

const (
	digestA = "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23"
	digestB = "cc0b4e42510d49c6decd464123ecf3b14ae9b47f9b4ed2ee64893e2d6520a264"
)

func TestAPinBelongsToAVersion(t *testing.T) {
	m := map[string]any{"1.3.1": digestA, "1.3": digestB}

	got, err := pinnedDigest(m, "1.3.1")
	if err != nil {
		t.Fatal(err)
	}
	if got != digestA {
		t.Errorf("resolved %q for 1.3.1, want the digest pinned for it", got)
	}
	if got, _ := pinnedDigest(m, "1.3"); got != digestB {
		t.Errorf("resolved %q for 1.3, want the other version's digest", got)
	}
}

// ⛔⛔ OPTING IN AND THEN SKIPPING IS HOW A CONTROL BECOMES DECORATION. A
// recipe that pins has vouched for the versions it names and for no others,
// so a version with no entry is refused rather than fetched unverified. The
// refusal names which versions ARE pinned, because the usual cause is a
// version bump that nobody pinned.
func TestAnUnpinnedVersionIsRefusedWhenTheRecipePins(t *testing.T) {
	_, err := pinnedDigest(map[string]any{"1.3.1": digestA, "1.3": digestB}, "1.4.0")
	if err == nil {
		t.Fatal("built an unpinned version against a recipe that pins")
	}
	for _, want := range []string{"1.4.0", "1.3", "1.3.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestARecipeThatPinsNothingIsUnaffected(t *testing.T) {
	got, err := pinnedDigest(nil, "1.3.1")
	if err != nil || got != "" {
		t.Errorf("pinnedDigest(nil) = %q, %v; want no pin and no error", got, err)
	}
}

// A pin that is almost a digest reads as a control and compares against
// nothing real, so the shape is checked rather than assumed.
func TestSomethingThatIsNotADigestIsRefused(t *testing.T) {
	for _, c := range []struct {
		what string
		v    any
	}{
		{"too short", strings.TrimSuffix(digestA, "3")},
		{"uppercase", strings.ToUpper(digestA)},
		{"not hex", strings.Replace(digestA, "9", "z", 1)},
		{"a bare URL", "https://example.org/x.tar.gz.sha256"},
		{"a number", 42},
	} {
		if _, err := pinnedDigest(c.v, "1.3.1"); err == nil {
			t.Errorf("%s was accepted as a pinned digest", c.what)
		}
		if _, err := pinnedDigest(map[string]any{"1.3.1": c.v}, "1.3.1"); err == nil {
			t.Errorf("%s was accepted as a pinned digest inside the map", c.what)
		}
	}
}

func TestTheScalarFormPinsTheOnlyVersion(t *testing.T) {
	got, err := pinnedDigest(digestA, "anything")
	if err != nil {
		t.Fatal(err)
	}
	if got != digestA {
		t.Errorf("resolved %q, want the scalar pin", got)
	}
}

func TestAMismatchRefusesAndShowsBothDigests(t *testing.T) {
	r := &Runner{}
	err := r.verifyChecksum(source{url: "https://example.org/x.tar.gz", sha256: digestA}, digestB)
	if err == nil {
		t.Fatal("accepted bytes that do not match the recipe's pin")
	}
	for _, want := range []string{digestA, digestB, "pinned by the recipe", "actually arrived"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not show %q: %v", want, err)
		}
	}
}

func TestMatchingBytesPass(t *testing.T) {
	r := &Runner{}
	if err := r.verifyChecksum(source{url: "https://example.org/x.tar.gz", sha256: digestA}, digestA); err != nil {
		t.Errorf("refused bytes that match the pin: %v", err)
	}
}

// ⛔ A PIN ON A GIT CHECKOUT CAN NEVER BE CHECKED, so it is refused rather
// than skipped. Skipping it would leave a recipe that LOOKS pinned and is
// not — the worst of the three states.
func TestAPinOnAGitCheckoutIsRefused(t *testing.T) {
	r := &Runner{}
	err := r.verifyChecksum(source{url: "https://example.org/r", git: true, sha256: digestA}, "")
	if err == nil {
		t.Fatal("a git source accepted a pinned digest it can never satisfy")
	}
	if !strings.Contains(err.Error(), "git checkout") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// The pin needs no network: FetchSHA is nil here, which is what a build with
// no `sha:` looks like, and the pinned check still runs.
func TestThePinNeedsNoNetwork(t *testing.T) {
	r := &Runner{}
	if r.FetchSHA != nil {
		t.Fatal("this test is only meaningful with no FetchSHA")
	}
	if err := r.verifyChecksum(source{url: "u", sha256: digestA}, digestB); err == nil {
		t.Error("the pinned check was skipped because no checksum URL was configured")
	}
}

func TestSourcesOfResolvesThePinForTheVersionBeingBuilt(t *testing.T) {
	dist := map[string]any{
		"url":    "https://example.org/x-{{version.raw}}.tar.gz",
		"sha256": map[string]any{"1.3.1": digestA},
	}
	srcs, err := sourcesOf(dist, "1.3.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].sha256 != digestA {
		t.Fatalf("sourcesOf gave %+v, want the pin resolved for 1.3.1", srcs)
	}
	if _, err := sourcesOf(dist, "1.4.0", ""); err == nil {
		t.Error("sourcesOf built an unpinned version against a recipe that pins")
	}
}
