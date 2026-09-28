package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAzureReplayUnavailableStorageStartsAndRecovers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "obstructed")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	sent := make(chan struct{}, 1)
	spool, err := newAzureReplaySpool(azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20},
		func(context.Context, []byte) error {
			select {
			case sent <- struct{}{}:
			default:
			}
			return nil
		})
	if err != nil {
		t.Fatalf("runtime storage failure must not fail startup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = spool.close(ctx)
	})
	if stats := spool.stats(); stats.AccountingReady {
		t.Fatalf("unavailable storage reported healthy: %+v", stats)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err = spool.submit(ctx, []byte("{}\n"))
	cancel()
	if err == nil || spool.admissionFailures.Load() != 1 {
		t.Fatal("failed storage admission was not reported")
	}
	// Repair only this fixture file. No process restart or manual exporter reset.
	if err = os.Remove(root); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for !spool.stats().AccountingReady {
		if !waitJournalCatchup(ctx, 10*time.Millisecond) {
			t.Fatal("background initialization did not recover")
		}
	}
	if err = spool.submit(ctx, []byte("{}\n")); err != nil {
		t.Fatalf("storage did not recover: %v", err)
	}
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("recovered spool did not deliver")
	}
}
