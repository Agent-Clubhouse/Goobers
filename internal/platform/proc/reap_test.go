package proc

import (
	"context"
	"os"
	"testing"
)

// TestStartOrphanReaperOnlyInstallsAsContainerInit is the blast-radius guard for
// #3398: the reap loop exists for the shipped container, where the daemon is
// pid 1, and MUST stay invisible to a developer's local `goobers up` — which is
// an ordinary child of a shell, launchd, or systemd and has an init above it
// already doing the reaping. Running on every platform is deliberate: the
// guard is the reason this change is safe off linux too.
func TestStartOrphanReaperOnlyInstallsAsContainerInit(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test process is itself pid 1; the guard's false branch cannot be observed from here")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if StartOrphanReaper(ctx) {
		t.Fatalf("StartOrphanReaper installed a reap loop at pid %d, want installation only at pid 1", os.Getpid())
	}
}
