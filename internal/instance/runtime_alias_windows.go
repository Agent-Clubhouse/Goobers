//go:build windows

package instance

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func createLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Abs(scoped)
	if err != nil {
		return err
	}
	output, err := exec.Command("cmd", "/c", "mklink", "/J", legacy, filepath.Clean(target)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
