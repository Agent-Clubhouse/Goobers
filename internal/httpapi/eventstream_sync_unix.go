//go:build !windows

package httpapi

import (
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
)

func syncObservedFile(file *os.File, path string) error {
	if err := file.Sync(); err != nil {
		return err
	}
	return durability.SyncDir(filepath.Dir(path))
}
