package proc

import (
	"context"
	"testing"
)

func TestCleanupProbeAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := ProbeCleanup(ctx)
	if got.Code != "cleanup_probe_canceled" || got.Outcome != "unobservable" {
		t.Fatalf("canceled probe = %+v", got)
	}
}
