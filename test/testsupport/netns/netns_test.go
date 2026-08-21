package netns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

type fakeTB struct {
	skipped bool
	msg     string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Skipf(format string, args ...any) {
	f.skipped = true
	f.msg = fmt.Sprintf(format, args...)
}

func withStubs(t *testing.T, goosValue string, probeFn func(context.Context) error) *fakeTB {
	t.Helper()
	previousGOOS, previousProbe := goos, probe
	goos, probe = goosValue, probeFn
	t.Cleanup(func() { goos, probe = previousGOOS, previousProbe })
	return &fakeTB{}
}

func TestRequireIsolationNoopsOffLinux(t *testing.T) {
	fake := withStubs(t, "darwin", func(context.Context) error {
		t.Fatal("probe must not run on a non-Linux platform")
		return nil
	})
	RequireIsolation(context.Background(), fake)
	if fake.skipped {
		t.Fatalf("RequireIsolation skipped on darwin: %q", fake.msg)
	}
}

func TestRequireIsolationPassesWhenProbeSucceeds(t *testing.T) {
	fake := withStubs(t, "linux", func(context.Context) error { return nil })
	RequireIsolation(context.Background(), fake)
	if fake.skipped {
		t.Fatalf("RequireIsolation skipped despite a successful probe: %q", fake.msg)
	}
}

func TestRequireIsolationSkipsOnEPERM(t *testing.T) {
	probeErr := fmt.Errorf("executor: network isolation probe: %w", &os.PathError{
		Op: "fork/exec", Path: "/bin/true", Err: syscall.EPERM,
	})
	fake := withStubs(t, "linux", func(context.Context) error { return probeErr })
	RequireIsolation(context.Background(), fake)
	if !fake.skipped {
		t.Fatal("RequireIsolation did not skip on an EPERM probe failure")
	}
	if fake.msg == "" {
		t.Fatal("RequireIsolation skip reason is empty")
	}
}

func TestRequireIsolationLeavesOtherErrorsAlone(t *testing.T) {
	probeErr := errors.New("executor: network isolation probe: fork/exec /bin/true: no such file or directory")
	fake := withStubs(t, "linux", func(context.Context) error { return probeErr })
	RequireIsolation(context.Background(), fake)
	if fake.skipped {
		t.Fatalf("RequireIsolation skipped on a non-permission error: %q", fake.msg)
	}
}
