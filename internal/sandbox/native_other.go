//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"runtime"
)

func newNative() (Sandbox, error) {
	// Keep policy validation compiled and linted on unsupported platforms even
	// though no native wrapper can apply it here.
	_ = validate
	_ = resolveDirectory
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)
}
