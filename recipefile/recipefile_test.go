package recipefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const yml = "dependencies:\n  openssl.org: ^1.1\nbuild: make\nprovides:\n  - bin/x\n"
const hclSrc = "dependencies = { \"openssl.org\" = \"^3\" }\nbuild { script = [\"make\"] }\nprovides = [\"bin/x\"]\n"

// TestLoadReadsEitherFrontEnd — and both end at the same schema, which is why
// pantry/hcl round-trips through YAML rather than decoding on its own.
func TestLoadReadsEitherFrontEnd(t *testing.T) {
	p := t.TempDir()
	write(t, Dir(p, "a.org"), "package.yml", yml)
	write(t, Dir(p, "b.org"), "package.hcl", hclSrc)

	a, err := Load(p, "a.org")
	if err != nil || a.Dependencies["openssl.org"] != "^1.1" {
		t.Errorf("yml: %+v, %v", a, err)
	}
	b, err := Load(p, "b.org")
	if err != nil || b.Dependencies["openssl.org"] != "^3" {
		t.Errorf("hcl: %+v, %v", b, err)
	}
}

// HCL wins when a project has both: that project is mid-conversion, and the
// converted file is the one its author is working on.
func TestLoadPrefersHCLDuringAConversion(t *testing.T) {
	p := t.TempDir()
	d := Dir(p, "both.org")
	write(t, d, "package.yml", yml)
	write(t, d, "package.hcl", hclSrc)
	r, err := Load(p, "both.org")
	if err != nil {
		t.Fatal(err)
	}
	if r.Dependencies["openssl.org"] != "^3" {
		t.Errorf("want the HCL recipe, got %v", r.Dependencies)
	}
}

// TestErrNoRecipeIsNotAParseError.
//
// The factory SKIPs a project with no recipe — a pantry need not be complete,
// and a closure walk names projects that resolve from upstream — while a
// recipe that exists and does not parse is a recorded failure. Collapsing both
// into "could not load" would make a broken recipe vanish from the failures
// list instead of appearing in it.
func TestErrNoRecipeIsNotAParseError(t *testing.T) {
	p := t.TempDir()
	if _, err := Load(p, "absent.org"); !errors.Is(err, ErrNoRecipe) {
		t.Errorf("absent must be ErrNoRecipe, got %v", err)
	}
	// The message names every filename tried: "no such file …/package.yml" on
	// a pantry that may hold either one reads as a missing project.
	_, err := Load(p, "absent.org")
	for _, want := range []string{"absent.org", "package.hcl", "package.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q: %v", want, err)
		}
	}

	write(t, Dir(p, "broken.org"), "package.yml", "provides: 123\n")
	if _, e := Load(p, "broken.org"); e == nil || errors.Is(e, ErrNoRecipe) {
		t.Errorf("a malformed recipe must be an error and NOT ErrNoRecipe, got %v", e)
	}
}

// A tree walk asks about a NAME, not a project, and must accept both — this is
// what made depgaps blind to an HCL recipe while everything else built it.
func TestIsRecipe(t *testing.T) {
	for _, n := range Names {
		if !IsRecipe(n) {
			t.Errorf("IsRecipe(%q) = false", n)
		}
	}
	for _, n := range []string{"README.md", "package.yaml", "package.json", ""} {
		if IsRecipe(n) {
			t.Errorf("IsRecipe(%q) = true", n)
		}
	}
}

// LoadFile takes a path, as `bk build --recipe` does, and picks the front-end
// from the extension.
func TestLoadFile(t *testing.T) {
	p := t.TempDir()
	write(t, p, "package.hcl", hclSrc)
	write(t, p, "package.yml", yml)
	h, err := LoadFile(filepath.Join(p, "package.hcl"))
	if err != nil || h.Dependencies["openssl.org"] != "^3" {
		t.Errorf("hcl by path: %+v, %v", h, err)
	}
	y, err := LoadFile(filepath.Join(p, "package.yml"))
	if err != nil || y.Dependencies["openssl.org"] != "^1.1" {
		t.Errorf("yml by path: %+v, %v", y, err)
	}
	if _, err := LoadFile(filepath.Join(p, "gone.yml")); err == nil {
		t.Error("a missing path must fail")
	}
}

// TestLoadOverlayPrefersTheOverlay.
//
// The factory consults PKGX_PANTRY_OVERLAY before the pantry, so a tool that
// reads only the pantry describes a build nobody performs. The s390x seed
// order was computed that way and missed two projects — both of which were
// then discovered by a failed build, three steps downstream.
func TestLoadOverlayPrefersTheOverlay(t *testing.T) {
	pan, ov := t.TempDir(), t.TempDir()
	write(t, Dir(pan, "perl.org"), "package.yml", "build: make\n")
	write(t, Dir(ov, "perl.org"), "package.yml", "dependencies:\n  x.org/crypt: '*'\nbuild: make\n")
	write(t, Dir(pan, "only-upstream.org"), "package.yml", "dependencies:\n  a.org: ^1\nbuild: make\n")

	r, err := LoadOverlay(ov, pan, "perl.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Dependencies["x.org/crypt"]; !ok {
		t.Errorf("the overlay's recipe must win, got %v", r.Dependencies)
	}
	// A project the overlay does not carry falls through.
	u, err := LoadOverlay(ov, pan, "only-upstream.org")
	if err != nil || u.Dependencies["a.org"] != "^1" {
		t.Errorf("fall through to the pantry: %+v, %v", u, err)
	}
	// No overlay at all is the pantry-only case, so callers need not branch.
	if _, err := LoadOverlay("", pan, "perl.org"); err != nil {
		t.Errorf("an empty overlay must mean pantry-only: %v", err)
	}
}

// An overlay recipe that does not parse must FAIL, not fall through. Falling
// through would build what upstream says while the overlay says otherwise —
// silently, and the overlay exists precisely because upstream is wrong there.
func TestLoadOverlayDoesNotFallThroughAMalformedOverride(t *testing.T) {
	pan, ov := t.TempDir(), t.TempDir()
	write(t, Dir(pan, "a.org"), "package.yml", "build: make\n")
	write(t, Dir(ov, "a.org"), "package.yml", "provides: 123\n")
	if _, err := LoadOverlay(ov, pan, "a.org"); err == nil {
		t.Error("a broken override must be an error, not a silent fall-through")
	}
}
