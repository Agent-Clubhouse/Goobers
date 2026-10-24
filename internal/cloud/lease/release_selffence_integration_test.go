//go:build cloudintegration

package lease_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// offsetClock is a host clock the test can push forward.
type offsetClock struct{ offset atomic.Int64 }

func (c *offsetClock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *offsetClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

func selfFencingManager(t *testing.T, pool *pgxpool.Pool, clock *offsetClock) *lease.Manager {
	t.Helper()
	manager, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "holder-a",
		SelfFenceAfter: 2 * time.Second, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// requireHeldWithoutVoluntaryRelease asserts the handle still owns the lease
// and its epoch was not closed as a voluntary release.
func requireHeldWithoutVoluntaryRelease(t *testing.T, pool *pgxpool.Pool, handle *lease.Handle) {
	t.Helper()
	var holder string
	var epoch int64
	if err := pool.QueryRow(t.Context(), `
		SELECT holder, epoch FROM control.lease_heartbeat WHERE instance_id='instance' AND name='control'`,
	).Scan(&holder, &epoch); err != nil {
		t.Fatal(err)
	}
	if holder != handle.Holder || epoch != handle.Epoch {
		t.Fatalf("heartbeat = %s/%d, want the self-fenced holder's %s/%d kept until TTL", holder, epoch, handle.Holder, handle.Epoch)
	}
	var voluntary bool
	if err := pool.QueryRow(t.Context(), `
		SELECT release_kind IS NOT DISTINCT FROM 'voluntary' FROM control.lease_history
		WHERE instance_id='instance' AND name='control' AND epoch=$1`, handle.Epoch,
	).Scan(&voluntary); err != nil {
		t.Fatal(err)
	}
	if voluntary {
		t.Fatal("self-fenced handle recorded a voluntary release")
	}
}

// TestReleaseRefusesSelfFencedHandle: Release itself never hands over a lease
// whose holder has passed its self-fence window, whoever calls it.
func TestReleaseRefusesSelfFencedHandle(t *testing.T) {
	pool := testPool(t)
	clock := &offsetClock{}
	manager := selfFencingManager(t, pool, clock)
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(3 * time.Second)
	if err := manager.Release(t.Context(), handle); !errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("release of a self-fenced handle = %v, want ErrSelfFenced", err)
	}
	requireHeldWithoutVoluntaryRelease(t, pool, handle)
}

// TestReleaseRechecksSelfFenceAfterRowLock: a handle that is live when Release
// starts but self-fences while Release waits for the heartbeat row lock must
// still be refused once the lock is held.
func TestReleaseRechecksSelfFenceAfterRowLock(t *testing.T) {
	pool := testPool(t)
	clock := &offsetClock{}
	manager := selfFencingManager(t, pool, clock)
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := blocker.Exec(t.Context(), `
		SELECT 1 FROM control.lease_heartbeat WHERE instance_id='instance' AND name='control' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { released <- manager.Release(t.Context(), handle) }()
	waitForReleaseLock(t, pool)
	clock.advance(3 * time.Second)
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-released; !errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("release that self-fenced while waiting = %v, want ErrSelfFenced", err)
	}
	requireHeldWithoutVoluntaryRelease(t, pool, handle)
}

// waitForReleaseLock waits until a Release is blocked on the heartbeat row.
func waitForReleaseLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid() AND wait_event_type='Lock'
				  AND query LIKE '%control.lease_heartbeat%FOR UPDATE%'
			)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("release never reached the heartbeat row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReleaseIsBoundedBySelfFenceWindow: a release stuck behind the heartbeat
// row lock gives up when the holder's self-fence window runs out, as a fenced
// write does, rather than waiting to commit a handoff after its authority
// lapsed. The blocker is never released while Release runs.
func TestReleaseIsBoundedBySelfFenceWindow(t *testing.T) {
	pool := testPool(t)
	manager := selfFencingManager(t, pool, &offsetClock{})
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := blocker.Exec(t.Context(), `
		SELECT 1 FROM control.lease_heartbeat WHERE instance_id='instance' AND name='control' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { released <- manager.Release(t.Context(), handle) }()
	select {
	case err := <-released:
		if !errors.Is(err, lease.ErrSelfFenced) {
			t.Fatalf("release behind a held row lock = %v, want ErrSelfFenced at the window", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("release waited on the row lock past its self-fence window")
	}
}
