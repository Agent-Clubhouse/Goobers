//go:build !darwin && !linux && !windows

package journal

import (
	"fmt"
	"runtime"
)

func renameNoReplace(_, _ string) error {
	return fmt.Errorf("exclusive run publication is unsupported on %s", runtime.GOOS)
}
