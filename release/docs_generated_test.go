package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageReleaseDocsContainsOnlyGeneratedCLIReferences(t *testing.T) {
	payload, cleanup, err := stageReleaseDocs("v0.6.0-alpha.6", "test-commit", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	for _, sub := range []string{"cli", "man", "completion"} {
		dir := filepath.Join(payload, "docs", sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s contains no generated references", sub)
		}
		for _, entry := range entries {
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), releaseDocsGeneratorFixtureMarker) {
				t.Errorf("%s/%s was copied from source instead of generated for the released binary", sub, entry.Name())
			}
		}
	}
}
