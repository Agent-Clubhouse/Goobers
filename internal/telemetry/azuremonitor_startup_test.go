package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// Keep this as a deterministic admission invariant for a future cold-index
// implementation: reconstruction may hold the root index lock, but a newly
// emitted record must still be durably accepted on a separately bounded path.
// The current implementation does not satisfy this test; do not land the test
// alone or weaken its deadline to make an unsafe startup shortcut look green.
func TestAzureReplayColdIndexLockDoesNotBlockDurableStartupAdmission(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker, err := platformlock.TryAcquire(filepath.Join(root, ".replay-index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Release() }()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20}
	s, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.close(ctx)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := s.submit(ctx, []byte("{\"startup\":true}\n")); err != nil {
		t.Fatalf("cold index lock blocked durable startup admission: %v", err)
	}
}

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
