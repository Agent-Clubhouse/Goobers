package gaggletemplate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/lock"
)

// LockConfig serializes daemon edits and source replacement only for enrolled
// instances. The lock is outside config/ so directory swaps cannot replace it.
// Incoming configs include the first enrollment before it becomes visible.
func LockConfig(configDir string, incoming ...string) (func() error, error) {
	noop := func() error { return nil }
	for _, candidate := range append([]string{configDir}, incoming...) {
		managed, err := hasTracking(candidate)
		if err != nil {
			return nil, err
		}
		if !managed {
			continue
		}
		held, err := lock.TryAcquire(filepath.Join(filepath.Dir(configDir), ".template-config.lock"))
		if err != nil {
			return nil, err
		}
		return held.Release, nil
	}
	return noop, nil
}

func hasTracking(configDir string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(configDir, "gaggles"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(configDir, "gaggles", entry.Name(), MetadataDir)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
