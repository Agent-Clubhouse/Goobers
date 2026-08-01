//go:build !darwin && !linux && !windows

package executor

import (
	"fmt"
	"os/exec"
	"runtime"
)

func configureNoNetwork(*exec.Cmd) (marker string, err error) {
	return "", fmt.Errorf("executor: network mode %q is unsupported on %s", "none", runtime.GOOS)
}
