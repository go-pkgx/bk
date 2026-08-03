package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-pkgx/bk/config"
	"github.com/go-pkgx/bk/target"
)

// TestProofPropsInBuildDir is a focused end-to-end proof: a recipe whose script
// runs `test -f props/iconv.patch` succeeds only because the RecipeDir's props/
// was copied into the build tree the wrapped script cd's into.
func TestProofPropsInBuildDir(t *testing.T) {
	tenv(t)
	tgt, _ := target.Resolve()
	project := "gnu.org/tar"

	recipeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(recipeDir, "props"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recipeDir, "props", "iconv.patch"), []byte("PATCH"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := okRunner(project, tgt)
	r.RecipeDir = recipeDir
	// really run the generated (wrapped) script under bash, then create the
	// staging dir so the pipeline's rename step still works.
	r.Run = func(scriptPath string, env []string) error {
		cmd := exec.Command("/bin/bash", scriptPath)
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return os.MkdirAll(config.Compute(project, "1.2.3", tgt).BuildInstall, 0o755)
	}

	rec := okRecipe()
	rec.Distributable = nil // no fetch; the build dir is created empty by the pipeline
	rec.Dependencies = nil
	rec.Build = map[string]any{"script": []any{"test -f props/iconv.patch"}}

	if _, err := r.Build(rec, project, "*", tgt, tgt, ""); err != nil {
		t.Fatalf("proof failed - props not visible to script: %v", err)
	}
	t.Log("PROOF OK: `test -f props/iconv.patch` passed inside the build dir")
}
