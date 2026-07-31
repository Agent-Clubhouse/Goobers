package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveHarnessCommand bypasses npm's cmd.exe shim, which truncates a
// multiline --prompt argument at its first newline. The sibling PowerShell
// shim forwards the argument without lossy cmd.exe parsing.
func resolveHarnessCommand(command []string) []string {
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

// resolveStdioHarnessCommand resolves a harness command for a stdio JSON-RPC
// connection, and deliberately does NOT redirect to the PowerShell shim that
// resolveHarnessCommand selects.
//
// npm's generated .ps1 shim forwards stdin as:
//
//	if ($MyInvocation.ExpectingInput) { $input | & node ... }
//
// A stdio connection always attaches a pipe, so ExpectingInput is true and the
// stream is routed through PowerShell's $input enumerator — which is
// line-oriented, re-encoded, and buffered. The JSON-RPC handshake therefore
// never completes and the client blocks until its deadline. The .cmd shim
// hands the raw stdio streams to node untouched.
//
// The multiline-argument truncation that motivates the PowerShell shim cannot
// arise here: a stdio client carries its payload over the pipe, not in argv.
func resolveStdioHarnessCommand(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	resolved, err := exec.LookPath(command[0])
	if err != nil {
		return append([]string(nil), command...)
	}
	return append([]string{resolved}, command[1:]...)
}
