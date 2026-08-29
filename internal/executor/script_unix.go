//go:build !windows

package executor

func scriptCommand(script string) ([]string, []string, func(), error) {
	return []string{"sh", "-c", script}, nil, func() {}, nil
}

// commandInvocation is a no-op outside Windows: POSIX has no batch-file
// executable class requiring a shell wrapper to start reliably (see the
// Windows implementation in script_windows.go for what this works around).
func commandInvocation(name string, args []string) (string, []string) {
	return name, args
}
