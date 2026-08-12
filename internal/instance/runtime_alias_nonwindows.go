//go:build !windows

package instance

import (
	"os"
	"path/filepath"
)

func createLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Rel(filepath.Dir(legacy), scoped)
	if err != nil {
		return err
	}
	return os.Symlink(target, legacy)
}
