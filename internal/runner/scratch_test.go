package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReapScratchWorkspacesRemovesOwnedEntries(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, scratchWorkspacePrefix+"orphan")
	unowned := filepath.Join(root, "operator-notes")
	for _, path := range []string{orphan, unowned} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := ReapScratchWorkspaces(root); err != nil {
		t.Fatalf("ReapScratchWorkspaces: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists: %v", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatalf("unowned entry was removed: %v", err)
	}
}

func TestReapScratchWorkspacesAllowsMissingRoot(t *testing.T) {
	if err := ReapScratchWorkspaces(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("ReapScratchWorkspaces: %v", err)
	}
}
