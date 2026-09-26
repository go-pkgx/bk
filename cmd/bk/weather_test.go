package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/go-pkgx/bottle"
)

// A gap is an asymmetry WITHIN an OS. gnu.org/glibc is absent from darwin
// because darwin has Apple's libc; reporting that would bury the one finding
// that matters under the ones that cannot be otherwise. The first hand-run of
// this reported four gaps and every one was a category error.
func TestWeatherReportsOnlyWithinOSAsymmetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		have map[string][]bool
		want bool
	}{
		{"one arch of an OS is missing it", map[string][]bool{"darwin": {true, false}}, true},
		{"a whole OS lacks it", map[string][]bool{"darwin": {false, false}, "linux": {true, true}}, false},
		{"everywhere", map[string][]bool{"darwin": {true, true}, "linux": {true, true}}, false},
		{"nowhere", map[string][]bool{"darwin": {false, false}}, false},
	} {
		if got := asymmetric(tc.have); got != tc.want {
			t.Errorf("%s: asymmetric = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The gawk case, end to end: a pinned constraint satisfiable on one
// architecture of an OS and not the other is what killed the x86-64 lane, and
// it is what this must catch.
func TestWeatherCatchesThePinnedConstraint(t *testing.T) {
	old := weatherPick
	defer func() { weatherPick = old }()
	weatherPick = func(_ string, cons []string, osn, arch string) (bottle.Ver, error) {
		// 5.4.1 everywhere; 5.3.2 only on aarch64 — exactly the registry that
		// let darwin/x86-64 die silently.
		if len(cons) == 0 {
			return bottle.ParseVer("5.4.1"), nil
		}
		if osn == "darwin" && arch == "x86-64" {
			return bottle.Ver{}, errors.New("not published here")
		}
		return bottle.ParseVer("5.3.2"), nil
	}
	var out bytes.Buffer
	if rc := runWeather([]string{"gnu.org/gawk"}, &out, &out); rc != 0 {
		t.Errorf("unconstrained: rc = %d, want 0 — 5.4.1 is published everywhere", rc)
	}
	out.Reset()
	if rc := runWeather([]string{"gnu.org/gawk@~5.3"}, &out, &out); rc != 1 {
		t.Fatalf("pinned: rc = %d, want 1", rc)
	}
	if !strings.Contains(out.String(), "ASYMMETRIC") || !strings.Contains(out.String(), "darwin/x86-64=none") {
		t.Errorf("report:\n%s", out.String())
	}
}

// --quiet prints only what is wrong, for a sweep whose output is diffed.
func TestWeatherQuiet(t *testing.T) {
	old := weatherPick
	defer func() { weatherPick = old }()
	weatherPick = func(p string, _ []string, _, _ string) (bottle.Ver, error) {
		return bottle.ParseVer("1.0"), nil
	}
	var out bytes.Buffer
	runWeather([]string{"--quiet", "a.org"}, &out, &out)
	if strings.Contains(out.String(), "a.org ") {
		t.Errorf("--quiet printed a clean project:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "0 asymmetric") {
		t.Errorf("--quiet dropped the total:\n%s", out.String())
	}
}

// A platform with no slash is REFUSED, not guessed at. "linux" is not a
// platform, and answering for some default architecture would quietly be a
// different question.
func TestWeatherRefusesAPlatformThatIsNotOne(t *testing.T) {
	for _, p := range []string{"linux", "darwin/", "/x86-64", ""} {
		if _, err := parsePlatforms(p); err == nil {
			t.Errorf("parsePlatforms(%q) was accepted", p)
		}
	}
	got, err := parsePlatforms("darwin/aarch64, linux/s390x")
	if err != nil || len(got) != 2 || got[1].arch != "s390x" {
		t.Errorf("parsePlatforms = %v, %v", got, err)
	}
}

// `name@constraint`, so builder/toolchain.txt feeds this unchanged.
func TestSplitConstraint(t *testing.T) {
	for _, tc := range []struct {
		in, project, cons string
	}{
		{"perl.org@~5.44", "perl.org", "~5.44"},
		{"gnu.org/gawk", "gnu.org/gawk", ""},
		{"@weird", "@weird", ""}, // a leading @ is a name, not a constraint
	} {
		p, c := splitConstraint(tc.in)
		if p != tc.project || (tc.cons == "" && len(c) != 0) || (tc.cons != "" && (len(c) != 1 || c[0] != tc.cons)) {
			t.Errorf("splitConstraint(%q) = %q, %v", tc.in, p, c)
		}
	}
}

// Usage mistakes are usage mistakes.
func TestWeatherUsage(t *testing.T) {
	oldIn := unresolvedStdin
	defer func() { unresolvedStdin = oldIn }()
	unresolvedStdin = strings.NewReader("")
	var out bytes.Buffer
	if rc := runWeather(nil, &out, &out); rc != 2 {
		t.Errorf("no projects: rc = %d, want 2", rc)
	}
	if rc := runWeather([]string{"--platforms", "nonsense", "a.org"}, &out, &out); rc != 2 {
		t.Errorf("bad platform: rc = %d, want 2", rc)
	}
	if rc := runWeather([]string{"--nope"}, &out, &out); rc != 2 {
		t.Errorf("bad flag: rc = %d, want 2", rc)
	}
}

// A list that cannot be READ is not an empty list: saying "no projects" there
// blames the operator for somebody else's broken pipe.
func TestWeatherWhenTheListCannotBeRead(t *testing.T) {
	old := unresolvedStdin
	defer func() { unresolvedStdin = old }()
	unresolvedStdin = errReader{}
	var out bytes.Buffer
	if rc := runWeather(nil, &out, &out); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if !strings.Contains(out.String(), "weather:") {
		t.Errorf("the failure was not reported:\n%s", out.String())
	}
}

// Reached through the dispatch, like an operator would.
func TestWeatherThroughTheDispatch(t *testing.T) {
	old := weatherPick
	defer func() { weatherPick = old }()
	weatherPick = func(_ string, _ []string, osn, arch string) (bottle.Ver, error) {
		if osn == "darwin" && arch == "x86-64" {
			return bottle.Ver{}, errors.New("not published here")
		}
		return bottle.ParseVer("1.0"), nil
	}
	code, out, _ := run2(t, "weather", "a.org")
	if code != 1 {
		t.Errorf("exit = %d, want 1 when a platform is missing one", code)
	}
	if !strings.Contains(out, "ASYMMETRIC") {
		t.Errorf("output:\n%s", out)
	}
}
