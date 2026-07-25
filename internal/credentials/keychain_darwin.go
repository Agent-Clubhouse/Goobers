//go:build darwin

package credentials

import (
	"context"
	"fmt"
	"os/exec"
)

var runSecurity = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/security", args...).Output()
}

func resolveKeychain(ctx context.Context, service string) (string, error) {
	value, err := runSecurity(ctx, "find-generic-password", "-s", service, "-w")
	if err != nil {
		return "", fmt.Errorf("read macOS Keychain service %q: %w", service, err)
	}
	return string(value), nil
}
