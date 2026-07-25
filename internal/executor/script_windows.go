//go:build windows

package executor

import (
	"fmt"
	"os"
	"strings"
)

const inlineScriptPathEnv = "GOOBERS_INLINE_SCRIPT_PATH"

func scriptCommand(script string) ([]string, []string, func(), error) {
	file, err := os.CreateTemp("", "goobers-inline-*.cmd")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("executor: create inline script: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	script = strings.ReplaceAll(script, "\r\n", "\n")
	script = strings.ReplaceAll(script, "\n", "\r\n")
	if _, err := file.WriteString(script); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, nil, fmt.Errorf("executor: write inline script: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("executor: close inline script: %w", err)
	}
	// cmd.exe does not follow CommandLineToArgvW quoting. Expand a quoted
	// environment value so scripts and paths with quotes never enter argv.
	command := []string{"cmd.exe", "/D", "/S", "/C", "%" + inlineScriptPathEnv + "%"}
	env := []string{inlineScriptPathEnv + "=\"" + path + "\""}
	return command, env, cleanup, nil
}
