package testdep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

// stubUserNamespaceProbe swaps in a fake platform and probe for the duration of
// one test, so every branch of RequireUserNamespaces is exercisable on any
// build host and none of these tests need the capability to be present.
func stubUserNamespaceProbe(t *testing.T, platform string, probe func(context.Context) error) *fakeTB {
	t.Helper()
	previousGOOS, previousProbe := goos, userNamespaceProbe
	goos, userNamespaceProbe = platform, probe
	t.Cleanup(func() { goos, userNamespaceProbe = previousGOOS, previousProbe })
	return &fakeTB{}
}

func TestRequireUserNamespacesSkipsOffLinux(t *testing.T) {
	tb := stubUserNamespaceProbe(t, "darwin", func(context.Context) error {
		t.Fatal("probe must not run on a non-Linux platform")
		return nil
	})
	RequireUserNamespaces(tb)

	for _, want := range []string{"skipped", "CLONE_NEWUSER", "darwin"} {
		if !strings.Contains(tb.skip, want) {
			t.Errorf("skip message %q does not contain %q", tb.skip, want)
		}
	}
	if tb.fatal != "" {
		t.Fatalf("fatal=%q, want a declared skip", tb.fatal)
	}
	if tb.helpers == 0 {
		t.Fatal("RequireUserNamespaces did not mark itself as a test helper")
	}
}

func TestRequireUserNamespacesPassesWhenProbeSucceeds(t *testing.T) {
	tb := stubUserNamespaceProbe(t, "linux", func(context.Context) error { return nil })
	RequireUserNamespaces(tb)

	if tb.fatal != "" || tb.skip != "" {
		t.Fatalf("fatal=%q skip=%q, want neither — the capability is present", tb.fatal, tb.skip)
	}
	if tb.helpers == 0 {
		t.Fatal("RequireUserNamespaces did not mark itself as a test helper")
	}
}

func TestRequireUserNamespacesClassifiesProbeFailures(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantSkip bool
	}{
		{
			name: "clone denied by the security posture",
			err: fmt.Errorf("testdep: user namespace probe: %w", &os.PathError{
				Op: "fork/exec", Path: "/bin/true", Err: syscall.EPERM,
			}),
			wantSkip: true,
		},
		{
			name: "clone denied with EACCES",
			err: fmt.Errorf("testdep: user namespace probe: %w", &os.PathError{
				Op: "fork/exec", Path: "/bin/true", Err: syscall.EACCES,
			}),
			wantSkip: true,
		},
		{
			name:     "bare permission sentinel",
			err:      fmt.Errorf("testdep: user namespace probe: %w", os.ErrPermission),
			wantSkip: true,
		},
		{
			name: "probe binary missing",
			err: fmt.Errorf("testdep: user namespace probe: %w", &os.PathError{
				Op: "fork/exec", Path: "/bin/true", Err: syscall.ENOENT,
			}),
			wantSkip: false,
		},
		{
			name:     "unsupported platform",
			err:      fmt.Errorf("testdep: user namespace probe: %w", errors.ErrUnsupported),
			wantSkip: false,
		},
		{
			name:     "opaque environment failure",
			err:      errors.New("testdep: user namespace probe: exit status 1"),
			wantSkip: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tb := stubUserNamespaceProbe(t, "linux", func(context.Context) error { return test.err })
			RequireUserNamespaces(tb)

			if skipped := tb.skip != ""; skipped != test.wantSkip {
				t.Fatalf("skipped = %t (%q), want %t", skipped, tb.skip, test.wantSkip)
			}
			if tb.fatal != "" {
				t.Fatalf("fatal=%q, want no hard failure — a capability gap is never a test failure", tb.fatal)
			}
			if !test.wantSkip {
				return
			}
			for _, want := range []string{"CLONE_NEWUSER", "operation not permitted", "denies the capability"} {
				if !strings.Contains(tb.skip, want) {
					t.Errorf("skip message %q does not name %q", tb.skip, want)
				}
			}
		})
	}
}

// TestProbeUserNamespaceReturnsADecision runs the real platform probe. It
// asserts only that the probe terminates and reports a decision without
// panicking or disturbing the test process: whether the capability is present
// depends on the host, and requiring either answer would make this test fail on
// exactly the hardened runtimes #3397 is about.
func TestProbeUserNamespaceReturnsADecision(t *testing.T) {
	beforeUID, beforeGID := os.Getuid(), os.Getgid()

	ctx, cancel := context.WithTimeout(context.Background(), userNamespaceProbeTimeout)
	defer cancel()
	if err := probeUserNamespace(ctx); err != nil {
		t.Logf("user namespaces unavailable on this host: %v", err)
	} else {
		t.Log("user namespaces available on this host")
	}

	if afterUID, afterGID := os.Getuid(), os.Getgid(); afterUID != beforeUID || afterGID != beforeGID {
		t.Fatalf(
			"probe changed the test process credentials: uid %d->%d, gid %d->%d",
			beforeUID, afterUID, beforeGID, afterGID,
		)
	}
}
