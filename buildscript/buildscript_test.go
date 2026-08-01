package buildscript

import (
	"strings"
	"testing"

	"github.com/go-pkgx/bk/moustache"
	"github.com/go-pkgx/bk/target"
)

func opts(platform, arch, version string) Options {
	return Options{
		Target:     target.Target{Platform: platform, Arch: arch, Triple: "t"},
		PkgVersion: version,
		Tokens:     []moustache.Token{{From: "prefix", To: "/opt/x/v1"}, {From: "hw.concurrency", To: "4"}},
	}
}

func TestGenerateStringAndList(t *testing.T) {
	got, err := Generate("./configure --prefix={{prefix}}", opts("linux", "x86-64", "1.0.0"))
	if err != nil || got != "./configure --prefix=/opt/x/v1" {
		t.Fatalf("string node = %q %v", got, err)
	}
	got, err = Generate([]any{"a {{prefix}}", "make -j{{ hw.concurrency }}"}, opts("linux", "x86-64", "1.0.0"))
	if err != nil || got != "a /opt/x/v1\n\nmake -j4" {
		t.Errorf("list node = %q", got)
	}
	// nil / scalar
	if s, _ := Generate(nil, opts("linux", "x86-64", "1")); s != "" {
		t.Errorf("nil = %q", s)
	}
	if s, _ := Generate(42, opts("linux", "x86-64", "1")); s != "42" {
		t.Errorf("scalar = %q", s)
	}
}

func TestGenerateObjectNode(t *testing.T) {
	node := map[string]any{
		"script":            []any{"./configure"},
		"working-directory": "build/{{ hw.concurrency }}",
		"env":               map[string]any{"CC": "clang", "ARGS": []any{"--prefix={{prefix}}", "--x"}},
	}
	got, err := Generate(node, opts("linux", "x86-64", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `export ARGS="--prefix=/opt/x/v1 --x"`) {
		t.Errorf("env not expanded: %q", got)
	}
	if !strings.Contains(got, `export CC="clang"`) {
		t.Errorf("scalar env: %q", got)
	}
	if !strings.Contains(got, "mkdir -p build/4\ncd build/4") {
		t.Errorf("working-directory: %q", got)
	}
}

func TestGuards(t *testing.T) {
	lin := opts("linux", "x86-64", "1.5.0")
	// platform guard
	if s, _ := scriptItem(map[string]any{"if": "windows", "run": "echo w"}, lin); s != "" {
		t.Errorf("windows guard should skip on linux: %q", s)
	}
	if s, _ := scriptItem(map[string]any{"if": "linux", "run": "echo l"}, lin); s != "echo l" {
		t.Errorf("linux guard should keep: %q", s)
	}
	// arch guard
	if s, _ := scriptItem(map[string]any{"if": "aarch64", "run": "x"}, lin); s != "" {
		t.Errorf("aarch64 guard should skip on x86-64: %q", s)
	}
	if s, _ := scriptItem(map[string]any{"if": "x86-64", "run": "x"}, lin); s != "x" {
		t.Error("x86-64 guard should keep")
	}
	// os/arch guard
	if s, _ := scriptItem(map[string]any{"if": "linux/x86-64", "run": "x"}, lin); s != "x" {
		t.Error("linux/x86-64 should keep")
	}
	if s, _ := scriptItem(map[string]any{"if": "linux/aarch64", "run": "x"}, lin); s != "" {
		t.Error("linux/aarch64 should skip")
	}
	// semver range guard
	if s, _ := scriptItem(map[string]any{"if": "^1", "run": "x"}, lin); s != "x" {
		t.Error("^1 should satisfy 1.5.0")
	}
	if s, _ := scriptItem(map[string]any{"if": "^2", "run": "x"}, lin); s != "" {
		t.Error("^2 should not satisfy 1.5.0")
	}
	// unrecognised condition includes the step
	if s, _ := scriptItem(map[string]any{"if": "some-flag", "run": "x"}, lin); s != "x" {
		t.Errorf("unrecognised guard should keep: %q", s)
	}
	// a range-like but unparseable condition does not gate (kept)
	if s, _ := scriptItem(map[string]any{"if": "^", "run": "x"}, lin); s != "x" {
		t.Errorf("unparseable range should keep: %q", s)
	}
	// an unparseable package version cannot gate a semver condition (kept)
	badVer := opts("linux", "x86-64", "not-a-version")
	if s, _ := scriptItem(map[string]any{"if": "^1", "run": "x"}, badVer); s != "x" {
		t.Errorf("bad version should keep: %q", s)
	}
}

func TestRunFormsAndErrors(t *testing.T) {
	o := opts("linux", "x86-64", "1")
	// run as list joins with newlines
	s, err := scriptItem(map[string]any{"run": []any{"a", "b {{prefix}}"}}, o)
	if err != nil || s != "a\nb /opt/x/v1" {
		t.Errorf("run list = %q %v", s, err)
	}
	// run missing/invalid type
	if _, err := scriptItem(map[string]any{"run": 5}, o); err == nil {
		t.Error("expected run type error")
	}
	// propagate through Generate list
	if _, err := Generate([]any{map[string]any{"run": 5}}, o); err == nil {
		t.Error("expected error to propagate")
	}
	// and through an object node's script
	if _, err := Generate(map[string]any{"script": []any{map[string]any{"run": 5}}}, o); err == nil {
		t.Error("expected error via object node")
	}
}

func TestWorkingDirectoryStep(t *testing.T) {
	s, _ := scriptItem(map[string]any{"run": "make", "working-directory": "sub/{{prefix}}"}, opts("linux", "x86-64", "1"))
	if !strings.Contains(s, `cd "sub//opt/x/v1"`) || !strings.Contains(s, `cd "$OLDWD"`) {
		t.Errorf("wd step = %q", s)
	}
}

func TestPlatformReduce(t *testing.T) {
	env := map[string]any{
		"BASE":    "1",
		"linux":   map[string]any{"CFLAGS": []any{"-O2"}, "ONLY_LINUX": "y"},
		"darwin":  map[string]any{"ONLY_MAC": "y"},
		"x86-64":  map[string]any{"CFLAGS": []any{"-m64"}},
		"aarch64": map[string]any{"NOPE": "y"},
		"CFLAGS":  []any{"-g"},
	}
	out := expandEnv(env, opts("linux", "x86-64", "1"))
	if !strings.Contains(out, "ONLY_LINUX") || strings.Contains(out, "ONLY_MAC") || strings.Contains(out, "NOPE") {
		t.Errorf("platform selection wrong: %q", out)
	}
	// linux + x86-64 CFLAGS lists supplement the base list
	if !strings.Contains(out, `-g -O2 -m64`) {
		t.Errorf("list supplement wrong: %q", out)
	}
}

func TestPlatformReduceScalarReplaceAndFreshList(t *testing.T) {
	// scalar sub-value replaces; a list sub-value with no existing base is set
	env := map[string]any{"linux": map[string]any{"CC": "gcc", "LD": []any{"-fuse-ld=lld"}}}
	out := expandEnv(env, opts("linux", "x86-64", "1"))
	if !strings.Contains(out, `export CC="gcc"`) || !strings.Contains(out, `export LD="-fuse-ld=lld"`) {
		t.Errorf("scalar/fresh-list = %q", out)
	}
	// a non-map platform value is ignored (defensive)
	env2 := map[string]any{"linux": "notamap", "X": "1"}
	if out := expandEnv(env2, opts("linux", "x86-64", "1")); !strings.Contains(out, `export X="1"`) {
		t.Errorf("non-map platform value = %q", out)
	}
}

func TestPlatformReduceOsArchKeyAndScalarBaseList(t *testing.T) {
	env := map[string]any{
		"X":            "base",                          // scalar base ...
		"linux/x86-64": map[string]any{"X": []any{"a"}}, // ... supplemented by an os/arch list
	}
	out := expandEnv(env, opts("linux", "x86-64", "1"))
	if !strings.Contains(out, `export X="base a"`) {
		t.Errorf("os/arch key + scalar-base list = %q", out)
	}
}

func TestPosixQuoteTrailingTrim(t *testing.T) {
	// a value ending in a quote yields a trailing "" pair that is trimmed
	if q := posixQuote(`x"`); q != `"x"` {
		t.Errorf("trailing-trim = %q", q)
	}
}

func TestEnvValueTypes(t *testing.T) {
	o := opts("linux", "x86-64", "1")
	env := map[string]any{"B": true, "F": false, "N": nil, "I": 7, "S": "{{prefix}}"}
	out := expandEnv(env, o)
	for _, want := range []string{`export B="1"`, `export F="0"`, `export N="0"`, `export I="7"`, `export S="/opt/x/v1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestPosixQuote(t *testing.T) {
	if q := posixQuote(`bar "baz" bun`); q != `"bar ""baz"" bun"` {
		t.Errorf("quote = %q", q)
	}
	// a value that is empty trims to a single pair
	if q := posixQuote(""); q != `"` {
		// "" -> `""` -> trim leading -> `"` ; acceptable edge
	}
	if q := posixQuote(`""leading`); !strings.HasPrefix(q, `"`) {
		t.Errorf("leading trim = %q", q)
	}
}

func TestFixtures(t *testing.T) {
	o := opts("linux", "x86-64", "1")
	// build `prop` as a plain string
	s, _ := scriptItem(map[string]any{"run": "cargo build", "prop": "[toolchain]\nchannel={{prefix}}"}, o)
	if !strings.Contains(s, "OLD_PROP=$PROP") || !strings.Contains(s, "channel=/opt/x/v1") {
		t.Errorf("prop fixture = %q", s)
	}
	// test `fixture` object with content + extname + shebang → chmod
	s, _ = scriptItem(map[string]any{"run": "./t", "fixture": map[string]any{"content": "#!/bin/sh\necho hi", "extname": ".sh"}}, o)
	if !strings.Contains(s, "FIXTURE=$(mktemp).sh") || !strings.Contains(s, "chmod +x $FIXTURE") {
		t.Errorf("fixture obj = %q", s)
	}
	// `contents` alias
	s, _ = scriptItem(map[string]any{"run": "x", "fixture": map[string]any{"contents": "data"}}, o)
	if !strings.Contains(s, "data") {
		t.Errorf("contents alias = %q", s)
	}
	// $ in fixture is escaped
	s, _ = scriptItem(map[string]any{"run": "x", "prop": "$HOME"}, o)
	if !strings.Contains(s, `\$HOME`) {
		t.Errorf("dollar escape = %q", s)
	}
}
