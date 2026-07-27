//go:build !darwin

package credentials

import (
	"context"
	"fmt"
	"runtime"
)

func resolveKeychain(_ context.Context, service string) (string, error) {
	return "", fmt.Errorf("macOS Keychain service %q is not supported on %s", service, runtime.GOOS)
}
