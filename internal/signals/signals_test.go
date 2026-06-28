package signals

import (
	"syscall"
	"testing"
	"time"
)

func TestSetupSignalContext_StopCancels(t *testing.T) {
	ctx, stop := SetupSignalContext()
	if ctx.Err() != nil {
		t.Fatal("context should be live before stop")
	}
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context not cancelled after stop()")
	}
}

func TestSetupSignalContext_SignalCancels(t *testing.T) {
	ctx, stop := SetupSignalContext()
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Skipf("cannot raise SIGTERM in this environment: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context not cancelled after SIGTERM")
	}
}
