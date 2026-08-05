package moustache

import (
	"reflect"
	"testing"
)

func find(toks []Token, from string) (string, bool) {
	for _, t := range toks {
		if t.From == from {
			return t.To, true
		}
	}
	return "", false
}

func TestApply(t *testing.T) {
	toks := []Token{{"prefix", "/opt/pkgx/x/v1"}, {"hw.concurrency", "8"}}
	in := "./configure --prefix={{prefix}} && make -j{{ hw.concurrency }}"
	want := "./configure --prefix=/opt/pkgx/x/v1 && make -j8"
	if got := Apply(in, toks); got != want {
		t.Errorf("Apply = %q", got)
	}
	// leading $ before {{...}} is consumed
	if got := Apply("${{prefix}}/bin", toks); got != "/opt/pkgx/x/v1/bin" {
		t.Errorf("leading-$ = %q", got)
	}
	// a To containing $ is inserted literally (no group-ref interpretation)
	if got := Apply("{{k}}", []Token{{"k", "$HOME/x"}}); got != "$HOME/x" {
		t.Errorf("literal-$ To = %q", got)
	}
	// project names with regex metachars (dots) are quoted
	if got := Apply("{{deps.openssl.org.prefix}}", []Token{{"deps.openssl.org.prefix", "/o"}}); got != "/o" {
		t.Errorf("dotted token = %q", got)
	}
}

func TestVersion(t *testing.T) {
	toks := Version("1.2.3", "version")
	checks := map[string]string{
		"version":           "1.2.3",
		"version.major":     "1",
		"version.minor":     "2",
		"version.patch":     "3",
		"version.marketing": "1.2",
		"version.raw":       "1.2.3",
		"version.build":     "",
	}
	for k, want := range checks {
		if got, ok := find(toks, k); !ok || got != want {
			t.Errorf("%s = %q,%v want %q", k, got, ok, want)
		}
	}
	if _, ok := find(toks, "version.tag"); ok {
		t.Error("no tag expected for a plain version")
	}
}

func TestVersionBuildAndTag(t *testing.T) {
	toks := Version("v1.1.1w+deb-3", "version")
	// strips leading v; build is everything after '+' (may itself contain '-')
	if got, _ := find(toks, "version.build"); got != "deb-3" {
		t.Errorf("build = %q", got)
	}
	// short version pads missing components with 0
	short := Version("2", "v")
	if got, _ := find(short, "v"); got != "2.0.0" {
		t.Errorf("short = %q", got)
	}
	if got, _ := find(short, "v.marketing"); got != "2.0" {
		t.Errorf("short marketing = %q", got)
	}
}

// TestVersionTag proves Version no longer derives a version.tag from a
// pre-release suffix: a "-rc1" is dropped from the numeric fields (raw = the
// bare core), and NO version.tag token is emitted — the real {{version.tag}}
// (the upstream git tag) is supplied by the build runner, not here.
func TestVersionTag(t *testing.T) {
	toks := Version("1.2.3-rc1", "version")
	if _, ok := find(toks, "version.tag"); ok {
		t.Error("Version must not emit version.tag (runner supplies the git tag)")
	}
	if got, _ := find(toks, "version.raw"); got != "1.2.3" {
		t.Errorf("raw core = %q", got)
	}
}

func TestDeps(t *testing.T) {
	toks := Deps([]Dep{{Project: "openssl.org", Version: "1.1.1", Path: "/opt/openssl/v1.1.1"}})
	if got, _ := find(toks, "deps.openssl.org.prefix"); got != "/opt/openssl/v1.1.1" {
		t.Errorf("dep prefix = %q", got)
	}
	if got, _ := find(toks, "deps.openssl.org.version.major"); got != "1" {
		t.Errorf("dep version.major = %q", got)
	}
}

func TestPrefixAndHost(t *testing.T) {
	if !reflect.DeepEqual(Prefix("/p"), []Token{{"prefix", "/p"}}) {
		t.Error("Prefix")
	}
	h := Host("x86-64", "x86_64-w64-mingw32", "windows", 4)
	if got, _ := find(h, "hw.target"); got != "x86_64-w64-mingw32" {
		t.Errorf("hw.target = %q", got)
	}
	if got, _ := find(h, "hw.concurrency"); got != "4" {
		t.Errorf("hw.concurrency = %q", got)
	}
}
