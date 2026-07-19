//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"runtime"
)

func newNative() (Sandbox, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)
}
