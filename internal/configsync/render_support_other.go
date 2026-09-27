//go:build !linux && !darwin

package configsync

import (
	"errors"
	"fmt"
	"runtime"
)

var errAtomicManifestPublicationUnsupported = errors.New("atomic manifest publication is unsupported")

func validateManifestPublicationSupport() error {
	return fmt.Errorf("%w on %s; render on Linux or macOS, or use --apply", errAtomicManifestPublicationUnsupported, runtime.GOOS)
}
