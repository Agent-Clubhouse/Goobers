//go:build !linux && !darwin

package configsync

import (
	"errors"
	"runtime"
)

func swapManifestPaths(_, _ string) error {
	return errors.New("atomic directory exchange is unsupported on " + runtime.GOOS)
}
