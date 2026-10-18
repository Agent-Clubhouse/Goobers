//go:build !windows

package harness

func resolveHarnessCommand(command []string) []string {
	return append([]string(nil), command...)
}

// resolveStdioHarnessCommand mirrors resolveHarnessCommand. Only Windows has a
// shim to choose between; elsewhere the command is already the real executable.
func resolveStdioHarnessCommand(command []string) []string {
	return append([]string(nil), command...)
}
