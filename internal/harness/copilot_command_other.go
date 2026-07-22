//go:build !windows

package harness

func resolveCopilotCommand(command []string) []string {
	return append([]string(nil), command...)
}
