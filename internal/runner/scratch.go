package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const scratchWorkspacePrefix = "stage-"

// ReapScratchWorkspaces removes scratch workspaces left by a prior daemon
// process. The daemon calls it while holding the instance lock, before
// resuming interrupted runs, so every owned entry is a crash orphan.
func ReapScratchWorkspaces(root string) error {
	if root == "" {
		return fmt.Errorf("runner: scratch workspace root is required")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("runner: list scratch workspaces: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), scratchWorkspacePrefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("runner: reap scratch workspace %s: %w", entry.Name(), err)
		}
	}
	return nil
}
