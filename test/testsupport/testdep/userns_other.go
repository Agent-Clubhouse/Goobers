//go:build !linux

package testdep

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// probeUserNamespace is unreachable in normal use — RequireUserNamespaces skips
// before it ever probes on a non-Linux platform — but it keeps the package
// building everywhere and gives the goos-injecting unit tests a well-defined
// result to hit. The error is deliberately not permission-shaped: an absent
// kernel feature is not a denied capability.
func probeUserNamespace(context.Context) error {
	return fmt.Errorf(
		"testdep: user namespace probe: %w: %s has no CLONE_NEWUSER",
		errors.ErrUnsupported, runtime.GOOS,
	)
}
