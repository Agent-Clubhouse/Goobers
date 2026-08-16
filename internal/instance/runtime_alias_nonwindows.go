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

// resolveRuntimeAlias reports the target a compatibility alias points at. Off
// Windows the alias is a plain symlink, so EvalSymlinks already does the job;
// see the Windows implementation for why that is not portable.
func resolveRuntimeAlias(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func createLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Rel(filepath.Dir(legacy), scoped)
	if err != nil {
		return err
	}
	return os.Symlink(target, legacy)
}
