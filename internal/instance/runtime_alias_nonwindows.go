//go:build !windows

package instance

import (
	"io/fs"
	"os"
	"path/filepath"
)

func isLegacyRuntimeAlias(_ string, info fs.FileInfo) (bool, error) {
	return info.Mode()&os.ModeSymlink != 0, nil
}

func createLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Rel(filepath.Dir(legacy), scoped)
	if err != nil {
		return err
	}
	return os.Symlink(target, legacy)
}
