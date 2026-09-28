package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/overrides"
)

func ov(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := runOverrides(args, &out, &errb)
	return rc, out.String(), errb.String()
}

// A pantry with one recipe and one patch that fits it.
func ovTree(t *testing.T, recipe, patch string) (dir, pantryDir string) {
	t.Helper()
	root := t.TempDir()
	dir, pantryDir = filepath.Join(root, "ov"), filepath.Join(root, "pantry")
	p := filepath.Join(pantryDir, "projects", "acme.org")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "package.yml"), []byte(recipe), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if patch != "" {
		if err := os.WriteFile(filepath.Join(dir, "acme.patch"), []byte(patch), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, pantryDir
}

const ovRecipe = "build:\n  script: make\n  env:\n    A: 1\n"

// A patch whose context still fits passes, and the count says how many were
// read — "0 skipped" out of nothing is the same sentence as "0 skipped" out of
// everything.
func TestOverridesAppliesCleanly(t *testing.T) {
	patch := `--- a/projects/acme.org/package.yml
+++ b/projects/acme.org/package.yml
@@ -1,4 +1,4 @@
 build:
   script: make
   env:
-    A: 1
+    A: 2
`
	dir, pantryDir := ovTree(t, ovRecipe, patch)
	rc, out, errb := ov(t, "--dir", dir, "--pantry", pantryDir)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %s", rc, errb)
	}
	if !strings.Contains(out, "1 patch override(s): 1 applied, 0 no longer apply") {
		t.Errorf("out = %q", out)
	}
}

// THE case this exists for, and the one that happened: a patch whose context
// no longer matches. The recipe is then the UNPATCHED one and the build fails
// for whatever the patch existed to remove — weeks later, somewhere else.
func TestOverridesFailsOnAPatchThatStoppedApplying(t *testing.T) {
	patch := `--- a/projects/acme.org/package.yml
+++ b/projects/acme.org/package.yml
@@ -1,4 +1,4 @@
 build:
   script: make
   env:
-    A: 99
+    A: 2
`
	dir, pantryDir := ovTree(t, ovRecipe, patch)
	rc, out, errb := ov(t, "--dir", dir, "--pantry", pantryDir)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 — a patch that stopped applying must fail the lane", rc)
	}
	if !strings.Contains(out, "1 patch override(s): 0 applied, 1 no longer apply") {
		t.Errorf("out = %q", out)
	}
	// Named by PROJECT as well as by patch: the patch name says which file was
	// meant to change, the project says what will be built wrong.
	if !strings.Contains(errb, "acme.org") || !strings.Contains(errb, "acme.patch") {
		t.Errorf("stderr must name the project and the patch: %q", errb)
	}
	if !strings.Contains(errb, "UNPATCHED") {
		t.Errorf("stderr must say what the consequence is: %q", errb)
	}
}

// An empty directory is a FAILURE, not a clean run. A --dir pointing at the
// wrong place would otherwise report success forever, which is the shape of
// every dead check in this repository's history.
func TestOverridesRefusesAnEmptyDirectory(t *testing.T) {
	dir, pantryDir := ovTree(t, ovRecipe, "")
	rc, _, errb := ov(t, "--dir", dir, "--pantry", pantryDir)
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	if !strings.Contains(errb, "nothing in") {
		t.Errorf("the message must say what it means: %q", errb)
	}
}

func TestOverridesErrors(t *testing.T) {
	if rc, _, _ := ov(t, "--nope"); rc != 2 {
		t.Errorf("an unknown flag must be a usage error, got %d", rc)
	}
	// The applier itself failing is reported, not mistaken for "nothing
	// skipped": a pantry that cannot be read is not a pantry that is clean.
	old := overridesApply
	t.Cleanup(func() { overridesApply = old })
	overridesApply = func(overrides.Options) (overrides.Result, error) {
		return overrides.Result{}, errors.New("boom")
	}
	rc, _, errb := ov(t, "--dir", t.TempDir(), "--pantry", t.TempDir())
	if rc != 1 || !strings.Contains(errb, "boom") {
		t.Errorf("rc = %d, stderr = %q", rc, errb)
	}
}

// Through the top-level dispatch, which is the only thing a CI lane invokes.
func TestOverridesDispatch(t *testing.T) {
	dir, pantryDir := ovTree(t, ovRecipe, `--- a/projects/acme.org/package.yml
+++ b/projects/acme.org/package.yml
@@ -1,4 +1,4 @@
 build:
   script: make
   env:
-    A: 1
+    A: 2
`)
	var out, errb bytes.Buffer
	if code := run([]string{"overrides", "--dir", dir, "--pantry", pantryDir}, &out, &errb); code != 0 {
		t.Errorf("code = %d, stderr = %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "1 applied") {
		t.Errorf("out = %q", out.String())
	}
}

// A subcommand that builds nothing must not need a build target.
//
// Resolving one before the dispatch meant `bk overrides` — and `bk lint`, and
// `bk tohcl` — refused to run on a machine whose BREWKIT_TARGET named a
// platform bk does not support, with an error about a platform the command
// never asked about. The factory SETS that variable, so a CI lane checking
// recipes beside a build inherits it.
func TestOverridesNeedsNoBuildTarget(t *testing.T) {
	t.Setenv("BREWKIT_TARGET", "plan9/vax")
	dir, pantryDir := ovTree(t, ovRecipe, `--- a/projects/acme.org/package.yml
+++ b/projects/acme.org/package.yml
@@ -1,4 +1,4 @@
 build:
   script: make
   env:
-    A: 1
+    A: 2
`)
	var out, errb bytes.Buffer
	if code := run([]string{"overrides", "--dir", dir, "--pantry", pantryDir}, &out, &errb); code != 0 {
		t.Errorf("code = %d, stderr = %q", code, errb.String())
	}
	// And the two subcommands that DO need a target still refuse it, because
	// for them the platform is the question rather than an accident of the
	// environment.
	for _, cmd := range [][]string{{"target"}, {"fixup", t.TempDir()}} {
		var o2, e2 bytes.Buffer
		if code := run(cmd, &o2, &e2); code == 0 {
			t.Errorf("bk %v must still refuse an unsupported platform: %q", cmd[0], o2.String())
		} else if !strings.Contains(e2.String(), "plan9") {
			t.Errorf("bk %v: the refusal must name the platform: %q", cmd[0], e2.String())
		}
	}
}

// writeHCL puts a logical override in the directory ovTree made.
func writeHCL(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const logicalRecipe = "distributable:\n  url: https://acme.org/{{version}}.tar.gz\ndependencies:\n  openssl.org: ^1.1\nbuild:\n  script: make install\nprovides:\n  - bin/acme\n"

// The three outcomes, which is why the logical format exists. A unified diff
// could only say "applies" or "does not", and "does not" covers two opposite
// situations — a defect, and a job upstream has already done.
func TestOverridesChecksTheLogicalOnes(t *testing.T) {
	t.Run("it is needed", func(t *testing.T) {
		dir, pantry := ovTree(t, logicalRecipe, "")
		writeHCL(t, dir, "acme.hcl", `
project = "acme.org"
why     = "our registry carries no openssl 1.x"
edits   = [{ path = "dependencies[\"openssl.org\"]", set = "^3" }]
`)
		rc, out, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 0 {
			t.Fatalf("rc = %d\n%s\n%s", rc, out, errb)
		}
		if !strings.Contains(out, "1 operation(s) applied, 0 already true") {
			t.Errorf("out:\n%s", out)
		}
	})

	t.Run("upstream has caught up", func(t *testing.T) {
		dir, pantry := ovTree(t, strings.Replace(logicalRecipe, "^1.1", "^3", 1), "")
		writeHCL(t, dir, "acme.hcl", `
project = "acme.org"
why     = "our registry carries no openssl 1.x"
edits   = [{ path = "dependencies[\"openssl.org\"]", set = "^3" }]
`)
		rc, out, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 0 {
			t.Fatalf("rc = %d\n%s\n%s", rc, out, errb)
		}
		// Not an error — an instruction. An override directory only ever grows
		// unless something says which entries have stopped being needed.
		if !strings.Contains(out, "upstream has caught up with — delete them") ||
			!strings.Contains(out, "acme.org (acme.hcl)") {
			t.Errorf("out:\n%s", out)
		}
	})

	t.Run("its premise is gone", func(t *testing.T) {
		dir, pantry := ovTree(t, logicalRecipe, "")
		writeHCL(t, dir, "acme.hcl", `
project = "acme.org"
why     = "w"
edits   = [{ path = "build.script", from = "cmake", to = "cmake3" }]
`)
		rc, _, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 1 {
			t.Fatalf("rc = %d\n%s", rc, errb)
		}
		if !strings.Contains(errb, "premise is gone") {
			t.Errorf("stderr:\n%s", errb)
		}
	})

	t.Run("it names a project the pantry has not got", func(t *testing.T) {
		dir, pantry := ovTree(t, logicalRecipe, "")
		writeHCL(t, dir, "absent.hcl", `
project = "absent.example"
why     = "w"
edits   = [{ path = "a", set = 1 }]
`)
		rc, _, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 1 || !strings.Contains(errb, "absent.example") {
			t.Errorf("rc = %d, stderr:\n%s", rc, errb)
		}
	})

	t.Run("it produces something a recipe may not say", func(t *testing.T) {
		dir, pantry := ovTree(t, logicalRecipe, "")
		writeHCL(t, dir, "acme.hcl", `
project = "acme.org"
why     = "w"
edits   = [{ path = "distributable", set = 7 }]
`)
		rc, _, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 1 {
			t.Errorf("a schema failure must stop the run: rc = %d\n%s", rc, errb)
		}
	})

	t.Run("a file that does not parse", func(t *testing.T) {
		dir, pantry := ovTree(t, logicalRecipe, "")
		writeHCL(t, dir, "bad.hcl", "project = ")
		rc, _, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 1 || !strings.Contains(errb, "overrides:") {
			t.Errorf("rc = %d, stderr:\n%s", rc, errb)
		}
	})

	t.Run("neither format present", func(t *testing.T) {
		dir, pantry := ovTree(t, logicalRecipe, "")
		rc, _, errb := ov(t, "--dir", dir, "--pantry", pantry)
		if rc != 1 || !strings.Contains(errb, "nothing in") {
			t.Errorf("rc = %d, stderr:\n%s", rc, errb)
		}
	})
}
