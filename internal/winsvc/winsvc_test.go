//go:build !windows

package winsvc

import (
	"context"
	"errors"
	"testing"
)

// TestIsWindowsServiceFalseOffWindows locks the invariant the daemon relies on:
// off Windows, IsWindowsService is always (false, nil), so runUp keeps its unix
// signal-driven shutdown path and never dispatches to the SCM handler.
func TestIsWindowsServiceFalseOffWindows(t *testing.T) {
	isSvc, err := IsWindowsService()
	if err != nil {
		t.Fatalf("IsWindowsService() error = %v, want nil", err)
	}
	if isSvc {
		t.Fatal("IsWindowsService() = true off Windows, want false")
	}
}

// TestRunUnsupportedOffWindows asserts the stub Run refuses without invoking fn.
// It is unreachable in production (callers gate on IsWindowsService first), but
// pinning it prevents a future caller from silently no-op-running the daemon.
func TestRunUnsupportedOffWindows(t *testing.T) {
	called := false
	code, err := Run("goobers", func(context.Context) int {
		called = true
		return 0
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Run() error = %v, want ErrUnsupported", err)
	}
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	if called {
		t.Fatal("Run() invoked fn off Windows, want fn untouched")
	}
}
