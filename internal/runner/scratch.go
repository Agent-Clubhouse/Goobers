package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/mutationsidecar"
)

const scratchWorkspacePrefix = "stage-"

// ReapScratchWorkspacesForRuns recovers receipts to the host-selected run root
// before removing crash orphans. Legacy receipts without an owner are retained.
func ReapScratchWorkspacesForRuns(root, runsDir string) error {
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
		path := filepath.Join(root, entry.Name())
		if _, err := os.Lstat(filepath.Join(path, scratchOwnerFile)); err == nil {
			if err := removeOwnedScratch(context.Background(), path, runsDir); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := mutationsidecar.RecoverBeforeCleanup(context.Background(), path, entry.Name(), "", ""); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("runner: reap scratch workspace %s: %w", entry.Name(), err)
		}
	}
	return nil
}
