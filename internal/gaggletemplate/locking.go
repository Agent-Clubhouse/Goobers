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
func LockConfig(configDir string) (func() error, error) {
	noop := func() error { return nil }
	entries, err := os.ReadDir(filepath.Join(configDir, "gaggles"))
	if errors.Is(err, fs.ErrNotExist) {
		return noop, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(configDir, "gaggles", entry.Name(), MetadataDir)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		held, err := lock.TryAcquire(filepath.Join(filepath.Dir(configDir), ".template-config.lock"))
		if err != nil {
			return nil, err
		}
		return held.Release, nil
	}
	return noop, nil
}
