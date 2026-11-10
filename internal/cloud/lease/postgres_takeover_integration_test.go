//go:build cloudintegration

package lease_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// TestPausedHolderCommitAfterTakeoverIsRejected resumes a holder that was
// paused mid-fenced-transaction only AFTER a successor has acquired the lease
// and committed its own fenced write, then has the stale holder attempt a real
// COMMIT. The stale commit must fail, its row must not persist, and a fresh
// fenced call on the stale handle must be refused without entering the callback.
func TestPausedHolderCommitAfterTakeoverIsRejected(t *testing.T) {
	pool := testPool(t)
	if _, err := pool.Exec(t.Context(), "CREATE TABLE control.takeover_zombie (holder text)"); err != nil {
		t.Fatal(err)
	}
	old := testManager(t, pool, "old")
	handle, err := old.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	resume := make(chan struct{})
	attempted := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- old.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO control.takeover_zombie VALUES ('old')"); err != nil {
				return err
			}
			close(entered)
			<-resume
			// Attempt COMMIT on resumption even with a context the fence did not bound.
			err := tx.Commit(context.Background())
			attempted <- err
			return err
		})
	}()
	<-entered

	expireHeartbeat(t, pool)
	successor := testManager(t, pool, "new")
	acquireCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	next, err := successor.Acquire(acquireCtx)
	if err != nil {
		close(resume)
		t.Fatal(err)
	}
	if err := successor.WithFence(t.Context(), next, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO control.takeover_zombie VALUES ('new')")
		return err
	}); err != nil {
		close(resume)
		t.Fatal(err)
	}

	close(resume)
	var commitErr error
	select {
	case commitErr = <-attempted:
	case <-time.After(10 * time.Second):
		t.Fatal("stale holder never attempted its commit")
	}
	if commitErr == nil {
		t.Fatalf("stale holder (epoch %d) committed after successor (epoch %d) took over", handle.Epoch, next.Epoch)
	}
	if err := <-done; err == nil {
		t.Fatal("WithFence reported success for a stale commit")
	}
	if err := old.WithFence(t.Context(), handle, func(context.Context, pgx.Tx) error {
		t.Error("stale handle entered its fenced callback")
		return nil
	}); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("fresh fenced call on stale handle = %v, want ErrStale", err)
	}
	var stale int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM control.takeover_zombie WHERE holder = 'old'").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("stale holder's write persisted (%d rows)", stale)
	}
}
