//go:build cloudintegration

package recoverypg_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/recoverypg"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/conformance/recoveryindex"
)

func TestRecordIndexConformance(t *testing.T) {
	recoveryindex.Run(t, func(t *testing.T) recoveryindex.Backend {
		pool := pgtest.Pool(t, "recoverypg")
		manager := pgtest.Manager(t, pool, "holder", 0)
		handle := pgtest.Acquire(t, manager)
		return recoveryindex.Backend{
			Open: func(*testing.T) recovery.RecordIndex {
				return recoverypg.Store{Pool: pool, Fence: manager}.Bind(handle)
			},
			StageBundle: func(_ *testing.T, data []byte) string {
				return fmt.Sprintf("bundles/%x", sha256.Sum256(data))
			},
		}
	})
}

func record(run string) recovery.Record {
	ref, _ := recovery.RefForRun(run)
	return recovery.Record{
		Version: 1, RunID: run, Ref: ref,
		RepositoryKey: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}.CanonicalKey(),
		BaseSHA:       strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40),
		PatchDigest: "sha256:" + strings.Repeat("c", 64), ArchiveDigest: "sha256:" + strings.Repeat("d", 64),
		ArchiveBytes: 42, ArchiveFormat: "delta",
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC),
	}
}

func TestRecoveryRetainUntilMonotone(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	// A host clock a year behind cannot shorten or skew database retention.
	manager := pgtest.Manager(t, pool, "holder", -365*24*time.Hour)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	r := record("run-1")
	published, err := store.Publish(t.Context(), handle, r, "bundles/run-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := pool.QueryRow(t.Context(), `
		SELECT retain_until = indexed_at + interval '1 hour' FROM control.recovery_record`).Scan(&exact); err != nil {
		t.Fatal(err)
	}
	if !exact {
		t.Fatal("retain_until is not exactly 1h past the database-clock publication time")
	}
	shorter, err := store.Renew(t.Context(), handle, r.Key(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !shorter.Record.RetainUntil.Equal(published.Record.RetainUntil) {
		t.Fatalf("shorter renewal moved retain_until from %s to %s", published.Record.RetainUntil, shorter.Record.RetainUntil)
	}
	longer, err := store.Renew(t.Context(), handle, r.Key(), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !longer.Record.RetainUntil.After(published.Record.RetainUntil) {
		t.Fatalf("longer renewal did not extend retain_until: %s", longer.Record.RetainUntil)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.recovery_record SET retain_until=now()-interval '1 day'"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(t.Context(), pgtest.InstanceID, r.Key())
	if err != nil {
		t.Fatal(err)
	}
	if !current.Record.RetainUntil.Equal(longer.Record.RetainUntil) {
		t.Fatalf("direct update shortened retain_until to %s", current.Record.RetainUntil)
	}
	for _, statement := range []string{
		"UPDATE control.recovery_record SET patch_digest='other'",
		"UPDATE control.recovery_record SET bundle_key='other'",
		"UPDATE control.recovery_record SET identity_unix_ns=identity_unix_ns+1",
	} {
		if _, err := pool.Exec(t.Context(), statement); err == nil {
			t.Fatalf("%s changed an immutable identity", statement)
		}
	}
}

func TestRecoveryConcurrentPublishResolvesToOneRecord(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	const callers = lease.MaxFencedConnections
	results := make(chan recovery.IndexedRecord, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			indexed, err := store.Publish(t.Context(), handle, record("run-1"), "bundles/run-1", time.Hour)
			if err != nil {
				t.Error(err)
			}
			results <- indexed
		})
	}
	wg.Wait()
	close(results)
	var first *recovery.IndexedRecord
	for indexed := range results {
		if first == nil {
			first = &indexed
		}
		if indexed != *first {
			t.Fatalf("concurrent publishes resolved to %+v and %+v", *first, indexed)
		}
	}
}

func TestRecoveryStaleOwnerAfterTakeover(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	oldManager := pgtest.Manager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	oldStore := recoverypg.Store{Pool: pool, Fence: oldManager}
	r := record("run-1")
	if _, err := oldStore.Publish(t.Context(), old, r, "bundles/run-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	newManager := pgtest.Manager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	newStore := recoverypg.Store{Pool: pool, Fence: newManager}
	if _, err := oldStore.Renew(t.Context(), old, r.Key(), 48*time.Hour); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale renew = %v, want ErrStale", err)
	}
	if _, err := oldStore.Publish(t.Context(), old, record("run-2"), "bundles/run-2", time.Hour); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale publish = %v, want ErrStale", err)
	}
	if _, err := oldStore.Reap(t.Context(), old, 10); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale reap = %v, want ErrStale", err)
	}
	if _, err := newStore.Renew(t.Context(), successor, r.Key(), 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := pool.QueryRow(t.Context(), "SELECT owner_epoch FROM control.recovery_record").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != successor.Epoch {
		t.Fatalf("owner epoch = %d, want successor %d", epoch, successor.Epoch)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.recovery_record SET owner_epoch=$1", old.Epoch); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT owner_epoch FROM control.recovery_record").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != successor.Epoch {
		t.Fatalf("owner epoch moved backwards to %d", epoch)
	}
}

func TestRecoveryReapUsesDatabaseClock(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	// The host clock is far ahead; only database-clock expiry is reaped.
	manager := pgtest.Manager(t, pool, "holder", 365*24*time.Hour)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	short, long := record("short"), record("long")
	if _, err := store.Publish(t.Context(), handle, short, "bundles/short", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), handle, long, "bundles/long", time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	reaped, err := store.Reap(t.Context(), handle, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Record.RunID != "short" || reaped[0].Bundle != "bundles/short" {
		t.Fatalf("reaped %+v, want only the expired record", reaped)
	}
	if _, err := store.Get(t.Context(), pgtest.InstanceID, long.Key()); err != nil {
		t.Fatal(err)
	}
}
