//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
	"sync"
	"testing"
)

func TestPostgresRecoverAfterSameProcessReacquire(t *testing.T) {
	base := postgresJournalConfigRenew(t, false)
	var mu sync.Mutex
	live := base.LiveOwner()
	config := journalpg.Config{Pool: base.Pool, InstanceID: base.InstanceID, Lease: base.Lease, LiveOwner: func() *lease.Handle { mu.Lock(); defer mu.Unlock(); return live }}
	backend := postgresBackend(t, config)
	ctx := context.Background()
	key := journal.RunKey{Gaggle: "g", RunID: "reacquired-run"}
	oldHandle, oldToken, err := backend.CreateRun(ctx, journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "implementation"}})
	if err != nil {
		t.Fatal(err)
	}
	// Lease lost and re-acquired by the same replica (e.g. one renew blip).
	if err := base.Lease.Release(ctx, live); err != nil {
		t.Fatal(err)
	}
	next, err := base.Lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Lease.Release(context.Background(), next) })
	mu.Lock()
	live = next
	mu.Unlock()
	if err := backend.Retire(ctx, oldHandle, oldToken); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("old Retire = %v", err)
	}
	recovered, owner, report, err := backend.RecoverRun(ctx, key, journal.RunWriterOptions{})
	t.Logf("epoch %d recover: last=%d err=%v", next.Epoch, report.LastSeq, err)
	if err != nil {
		t.Fatalf("same-process successor epoch cannot recover its run: %v", err)
	}
	if next.Epoch != 3 || report.LastSeq != 1 {
		t.Fatalf("epoch=%d report=%+v", next.Epoch, report)
	}
	if _, err := backend.AppendBatch(ctx, recovered, owner, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AppendBatch(ctx, oldHandle, oldToken, []journal.Event{{Type: journal.EventRunnerAnnotation}}); err == nil {
		t.Fatal("old handle survived same-process takeover")
	}
	if _, _, _, err := backend.RecoverRun(ctx, key, journal.RunWriterOptions{}); !errors.Is(err, journal.ErrRunOwned) {
		t.Fatalf("same-epoch duplicate recovery = %v", err)
	}
}
