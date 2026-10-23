//go:build cloudintegration

package recoverypg_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/recoverypg"
)

func record(runID string) recoverypg.Record {
	return recoverypg.Record{
		RunID: runID, RepositoryKey: "ado:org/app", Ref: "refs/goobers/recovery/" + runID,
		BaseRef: "refs/heads/main", BaseSHA: "base", SnapshotSHA: "snap", PatchDigest: "patch",
		ArchiveDigest: "archive", ArchiveBytes: 42, ArchiveFormat: "delta",
		BundleKey:    "recovery/" + runID + ".bundle",
		IdentityTime: time.Date(2026, 10, 1, 12, 0, 0, 123456000, time.UTC),
	}
}

func TestRecoveryRetainUntilMonotone(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	// A host clock a year behind cannot shorten or skew database retention.
	manager := pgtest.Manager(t, pool, "holder", -365*24*time.Hour)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	published, err := store.Publish(t.Context(), handle, record("run-1"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := published.RetainUntil.Sub(published.CreatedAt); got != time.Hour {
		t.Fatalf("retain_until - created_at = %s, want exactly 1h on the database clock", got)
	}
	shorter, err := store.Renew(t.Context(), handle, "run-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !shorter.RetainUntil.Equal(published.RetainUntil) {
		t.Fatalf("shorter renewal moved retain_until from %s to %s", published.RetainUntil, shorter.RetainUntil)
	}
	longer, err := store.Renew(t.Context(), handle, "run-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !longer.RetainUntil.After(published.RetainUntil) {
		t.Fatalf("longer renewal did not extend retain_until: %s", longer.RetainUntil)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.recovery_record SET retain_until=now()-interval '1 day'"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(t.Context(), pgtest.InstanceID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !current.RetainUntil.Equal(longer.RetainUntil) {
		t.Fatalf("direct update shortened retain_until to %s", current.RetainUntil)
	}
	for _, statement := range []string{
		"UPDATE control.recovery_record SET snapshot_sha='other'",
		"UPDATE control.recovery_record SET bundle_key='other'",
		"UPDATE control.recovery_record SET identity_time=now()",
	} {
		if _, err := pool.Exec(t.Context(), statement); err == nil {
			t.Fatalf("%s changed an immutable identity", statement)
		}
	}
}

func TestRecoveryPublishRetryIsIdempotent(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	const callers = lease.MaxFencedConnections
	results := make(chan recoverypg.Stored, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			stored, err := store.Publish(t.Context(), handle, record("run-1"), time.Hour)
			if err != nil {
				t.Error(err)
			}
			results <- stored
		})
	}
	wg.Wait()
	close(results)
	for stored := range results {
		if stored.Record != record("run-1") {
			t.Fatalf("retry resolved to %+v", stored.Record)
		}
	}
	changed := record("run-1")
	changed.SnapshotSHA = "different"
	if _, err := store.Publish(t.Context(), handle, changed, time.Hour); !errors.Is(err, recoverypg.ErrConflict) {
		t.Fatalf("different identity = %v, want ErrConflict", err)
	}
}

func TestRecoveryStaleOwnerAfterTakeover(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	oldManager := pgtest.Manager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	oldStore := recoverypg.Store{Pool: pool, Fence: oldManager}
	if _, err := oldStore.Publish(t.Context(), old, record("run-1"), time.Hour); err != nil {
		t.Fatal(err)
	}
	newManager := pgtest.Manager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	newStore := recoverypg.Store{Pool: pool, Fence: newManager}
	if _, err := oldStore.Renew(t.Context(), old, "run-1", 48*time.Hour); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale renew = %v, want ErrStale", err)
	}
	if _, err := oldStore.Publish(t.Context(), old, record("run-2"), time.Hour); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale publish = %v, want ErrStale", err)
	}
	if _, err := oldStore.Reap(t.Context(), old, 10); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale reap = %v, want ErrStale", err)
	}
	renewed, err := newStore.Renew(t.Context(), successor, "run-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.OwnerEpoch != successor.Epoch {
		t.Fatalf("owner epoch = %d, want successor %d", renewed.OwnerEpoch, successor.Epoch)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.recovery_record SET owner_epoch=$1", old.Epoch); err != nil {
		t.Fatal(err)
	}
	current, err := newStore.Get(t.Context(), pgtest.InstanceID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.OwnerEpoch != successor.Epoch {
		t.Fatalf("owner epoch moved backwards to %d", current.OwnerEpoch)
	}
}

func TestRecoveryReapUsesDatabaseClock(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	// The host clock is far ahead; only database-clock expiry is reaped.
	manager := pgtest.Manager(t, pool, "holder", 365*24*time.Hour)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	if _, err := store.Publish(t.Context(), handle, record("short"), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), handle, record("long"), time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	reaped, err := store.Reap(t.Context(), handle, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].RunID != "short" || reaped[0].BundleKey != "recovery/short.bundle" {
		t.Fatalf("reaped %+v, want only the expired record", reaped)
	}
	if _, err := store.Get(t.Context(), pgtest.InstanceID, "short"); !errors.Is(err, recoverypg.ErrNotFound) {
		t.Fatalf("reaped record still readable: %v", err)
	}
	if _, err := store.Get(t.Context(), pgtest.InstanceID, "long"); err != nil {
		t.Fatal(err)
	}
}
