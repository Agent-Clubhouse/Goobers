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

// ResolveRuntimeAlias reports the target a compatibility alias points at. Off
// Windows the alias is a plain symlink, so EvalSymlinks already does the job;
// see the Windows implementation for why that is not portable. Exported so
// other packages that scan a runtime tree containing such an alias (e.g.
// internal/telemetry/rollup, #3280) can dedupe against it without
// reimplementing platform-specific reparse-point resolution.
func ResolveRuntimeAlias(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// CreateLegacyRuntimeAlias creates the compatibility alias at legacy pointing
// at scoped — a plain symlink off Windows, a directory junction on Windows
// (see the Windows implementation). Exported alongside ResolveRuntimeAlias so
// a test can construct the platform-native alias this repo's own migration
// path creates, rather than a plain os.Symlink that isn't representative on
// Windows.
func CreateLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Rel(filepath.Dir(legacy), scoped)
	if err != nil {
		return err
	}
	return os.Symlink(target, legacy)
}
