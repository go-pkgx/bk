package target

import (
	"runtime"
	"strings"
	"testing"
)

func TestPkgxArch(t *testing.T) {
	cases := map[string]string{"amd64": "x86-64", "arm64": "aarch64", "riscv64": "riscv64"}
	for in, want := range cases {
		if got := pkgxArch(in); got != want {
			t.Errorf("pkgxArch(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHost(t *testing.T) {
	h := Host()
	if h.Platform != runtime.GOOS {
		t.Errorf("Host platform=%q want %q", h.Platform, runtime.GOOS)
	}
	if h.Triple == "" {
		t.Error("Host triple empty")
	}
	if h.Cross() {
		t.Error("Host must not be Cross()")
	}
	if h.Slug() != h.Platform+"/"+h.Arch {
		t.Errorf("Slug=%q", h.Slug())
	}
}

func TestNativeTripleAllOS(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "aarch64-apple-darwin"},
		{"darwin", "amd64", "x86_64-apple-darwin"},
		{"windows", "amd64", "x86_64-w64-mingw32"},
		{"linux", "amd64", "x86_64-unknown-linux-gnu"},
		{"linux", "arm64", "aarch64-unknown-linux-gnu"},
		{"linux", "riscv64", "riscv64-unknown-linux-gnu"},
	}
	for _, c := range cases {
		if got := nativeTripleFor(c.goos, c.goarch); got != c.want {
			t.Errorf("nativeTripleFor(%q,%q)=%q want %q", c.goos, c.goarch, got, c.want)
		}
	}
	// the runtime wrapper agrees with the pure fn for the current host
	if nativeTriple() != nativeTripleFor(runtime.GOOS, runtime.GOARCH) {
		t.Error("nativeTriple != nativeTripleFor(host)")
	}
}

func TestOverrideUnset(t *testing.T) {
	t.Setenv("BREWKIT_TARGET", "")
	_, _, ok, err := Override()
	if ok || err != nil {
		t.Errorf("unset: ok=%v err=%v", ok, err)
	}
	cross, err := IsCross()
	if cross || err != nil {
		t.Errorf("IsCross unset: %v %v", cross, err)
	}
	// Resolve falls back to host
	got, err := Resolve()
	if err != nil || got.Cross() {
		t.Errorf("Resolve unset=%+v err=%v", got, err)
	}
}

func TestOverrideWindows(t *testing.T) {
	for _, raw := range []string{"windows/x86-64", "windows+x86-64"} {
		t.Setenv("BREWKIT_TARGET", raw)
		p, a, ok, err := Override()
		if !ok || err != nil || p != "windows" || a != "x86-64" {
			t.Fatalf("%q → %q %q ok=%v err=%v", raw, p, a, ok, err)
		}
		tgt, err := Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if tgt.Platform != "windows" || tgt.Arch != "x86-64" || tgt.Triple != "x86_64-w64-mingw32" {
			t.Errorf("resolve=%+v", tgt)
		}
		if !tgt.Cross() && runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
			// only equal when host is windows/x86-64
		}
	}
	t.Setenv("BREWKIT_TARGET", "windows/aarch64")
	tgt, err := Resolve()
	if err != nil || tgt.Triple != "aarch64-w64-mingw32" {
		t.Errorf("win/aarch64 triple=%q err=%v", tgt.Triple, err)
	}
}

func TestOverrideNonWindowsTripleFallsBackToNative(t *testing.T) {
	t.Setenv("BREWKIT_TARGET", "linux/aarch64")
	tgt, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Platform != "linux" || tgt.Arch != "aarch64" {
		t.Errorf("resolve=%+v", tgt)
	}
	if tgt.Triple != nativeTriple() {
		t.Errorf("non-windows override should use native triple, got %q", tgt.Triple)
	}
}

func TestOverrideErrors(t *testing.T) {
	for _, bad := range []string{"garbage", "windows-x86-64", "plan9/x86-64", "windows/sparc"} {
		t.Setenv("BREWKIT_TARGET", bad)
		if _, _, _, err := Override(); err == nil {
			t.Errorf("%q: expected error", bad)
		}
		if _, err := Resolve(); err == nil {
			t.Errorf("%q: Resolve expected error", bad)
		}
		if _, err := IsCross(); err == nil {
			t.Errorf("%q: IsCross expected error", bad)
		}
	}
}

// TestOverrideS390x: naming an arch must not be stricter than running on it.
//
// Host() has no allowlist -- pkgxArch passes an unmapped GOARCH straight
// through -- so before s390x was added here, bk could build natively on a
// machine whose platform it refused to be told about. The factory always tells
// it (--platform linux/s390x), so the lane failed on a runner that was working.
func TestOverrideS390x(t *testing.T) {
	t.Setenv("BREWKIT_TARGET", "linux/s390x")
	tgt, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if tgt.Slug() != "linux/s390x" {
		t.Errorf("Slug() = %q, want linux/s390x", tgt.Slug())
	}
}

// TestOverrideGoSpellingIsDiagnosed: amd64 and arm64 are Go's names for two of
// the arches in the list, and the only arch names here that differ from Go's.
// A bare refusal sends the reader looking for a port that is already present.
func TestOverrideGoSpellingIsDiagnosed(t *testing.T) {
	for goName, pkgxName := range map[string]string{"amd64": "x86-64", "arm64": "aarch64"} {
		t.Setenv("BREWKIT_TARGET", "linux/"+goName)
		_, _, _, err := Override()
		if err == nil {
			t.Fatalf("Override() accepted linux/%s", goName)
		}
		if !strings.Contains(err.Error(), pkgxName) {
			t.Errorf("Override() for linux/%s = %v; want it to name %q", goName, err, pkgxName)
		}
	}
}

// TestWindowsArchWithoutTripleRefuses: windowsTriples is narrower than
// supportedArches now, and a map miss yields "" rather than failing. An empty
// triple does not stop anything here -- it travels to whichever build step
// first needs a compiler name, a long way from the decision that produced it.
func TestWindowsArchWithoutTripleRefuses(t *testing.T) {
	t.Setenv("BREWKIT_TARGET", "windows/s390x")
	tgt, err := Resolve()
	if err == nil {
		t.Fatalf("Resolve() = %+v, nil; want a refusal (triple was %q)", tgt, tgt.Triple)
	}
	if !strings.Contains(err.Error(), "s390x") {
		t.Errorf("Resolve() = %v; want the arch named", err)
	}
}

// TestEveryWindowsTripleIsASupportedArch is the other direction: a triple for
// an arch the allowlist rejects can never be reached, so it is dead weight
// that reads as support.
func TestEveryWindowsTripleIsASupportedArch(t *testing.T) {
	for arch := range windowsTriples {
		if !supportedArches[arch] {
			t.Errorf("windowsTriples has %q, which supportedArches refuses", arch)
		}
	}
}
