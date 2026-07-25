//go:build !windows

package harness

func resolveHarnessCommand(command []string) []string {
	return append([]string(nil), command...)
}
