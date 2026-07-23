package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveCopilotCommand bypasses npm's cmd.exe shim, which truncates a
// multiline --prompt argument at its first newline. The sibling PowerShell
// shim forwards the argument without lossy cmd.exe parsing.
func resolveCopilotCommand(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	resolved, err := exec.LookPath(command[0])
	if err != nil {
		return append([]string(nil), command...)
	}
	extension := strings.ToLower(filepath.Ext(resolved))
	if extension != ".cmd" && extension != ".bat" {
		return append([]string(nil), command...)
	}
	script := strings.TrimSuffix(resolved, filepath.Ext(resolved)) + ".ps1"
	if _, err := os.Stat(script); err != nil {
		return append([]string(nil), command...)
	}
	result := []string{
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-File",
		script,
	}
	return append(result, command[1:]...)
}
