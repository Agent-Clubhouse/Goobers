package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func activeRunIndexDir(runsDir string) string {
	return filepath.Join(filepath.Dir(runsDir), "."+filepath.Base(runsDir)+".active")
}

// MarkRunActive records a conservative recovery candidate before its journal
// becomes visible. The marker is removed only after terminal finalization.
func MarkRunActive(runsDir, runID string) (bool, error) {
	if !apiv1.ValidRunID(runID) {
		return false, fmt.Errorf("journal: invalid active run id %q", runID)
	}
	indexDir := activeRunIndexDir(runsDir)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return false, fmt.Errorf("journal: create active run index: %w", err)
	}
	path := filepath.Join(indexDir, runID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal: mark run %q active: %w", runID, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("journal: close active marker for %q: %w", runID, err)
	}
	if err := fsyncDir(indexDir); err != nil {
		return false, fmt.Errorf("journal: sync active run index: %w", err)
	}
	return true, nil
}

// ClearRunActive retires a recovery candidate after every terminal finalizer
// has completed successfully.
func ClearRunActive(runDir string) error {
	indexDir := activeRunIndexDir(filepath.Dir(runDir))
	err := os.Remove(filepath.Join(indexDir, filepath.Base(runDir)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("journal: clear active marker for %q: %w", filepath.Base(runDir), err)
	}
	return fsyncDir(indexDir)
}

// ActiveRunDirs returns the durable recovery candidates for one runs root.
func ActiveRunDirs(runsDir string) ([]string, error) {
	entries, err := os.ReadDir(activeRunIndexDir(runsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal: read active run index: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !apiv1.ValidRunID(entry.Name()) {
			continue
		}
		out = append(out, filepath.Join(runsDir, entry.Name()))
	}
	return out, nil
}
