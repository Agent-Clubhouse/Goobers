package journal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestErrorBackedProducersUseStructuredHelpers(t *testing.T) {
	root := repoRoot(t)
	re := regexp.MustCompile(`(?s)&(?:journal\.)?(?:ErrorDetail|apiv1\.ErrorInfo)\s*\{[^}]*Message\s*:\s*[A-Za-z_][A-Za-z0-9_]*\.Error\(\)`)
	allowed := map[string]bool{
		filepath.Clean("internal/journal/errorcause.go"): true,
	}
	for _, dir := range []string{"api", "cmd", "internal"} {
		base := filepath.Join(root, dir)
		if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.Clean(rel)
			if allowed[rel] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if loc := re.FindIndex(data); loc != nil {
				t.Fatalf("%s directly flattens an error into ErrorInfo/ErrorDetail near %q; use ErrorInfoFor/ErrorDetailFor", rel, data[loc[0]:loc[1]])
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found while locating repository root")
		}
		dir = parent
	}
}
