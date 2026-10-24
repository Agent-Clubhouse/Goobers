//go:build cloudintegration

package recoverypg_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/recoverypg"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/conformance/recoveryindex"
)

var staged atomic.Int64

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
				// Each publication stages under its own key, as object storage would.
				return fmt.Sprintf("bundles/%x/%d", sha256.Sum256(data), staged.Add(1))
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

// TestRecoveryConcurrentPublishResolvesToOneRecord races identical publishes.
// Each loser must resolve to the winner's record: a race on the bundle
// location's unique index once surfaced as ErrBundleUnavailable, so it runs
// many rounds to make that interleaving likely.
func TestRecoveryConcurrentPublishResolvesToOneRecord(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	const callers = lease.MaxFencedConnections
	for round := range 20 {
		run := fmt.Sprintf("run-%d", round)
		results := make(chan recovery.IndexedRecord, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Go(func() {
				indexed, err := store.Publish(t.Context(), handle, record(run), "bundles/"+run, time.Hour)
				if err != nil {
					t.Errorf("round %d: %v", round, err)
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
				t.Fatalf("round %d: concurrent publishes resolved to %+v and %+v", round, *first, indexed)
			}
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

// TestReapedBundleIsRetiredUntilReleased: between a reap and the caller's
// blob delete, a re-publication must not adopt the location being deleted.
func TestReapedBundleIsRetiredUntilReleased(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	r := record("run-1")
	if _, err := store.Publish(t.Context(), handle, r, "bundles/run-1", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if reaped, err := store.Reap(t.Context(), handle, 10); err != nil || len(reaped) != 1 {
		t.Fatalf("reap = %d, %v", len(reaped), err)
	}
	if _, err := store.Publish(t.Context(), handle, r, "bundles/run-1", time.Hour); !errors.Is(err, recovery.ErrBundleUnavailable) {
		t.Fatalf("re-publication into a reaped location = %v, want ErrBundleUnavailable", err)
	}
	if err := store.ReleaseBundle(t.Context(), handle, "bundles/run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), handle, r, "bundles/run-1", time.Hour); err != nil {
		t.Fatalf("re-publication after release: %v", err)
	}
}

// TestLostReapResultIsRecoveredFromTombstones (#8163): a Reap that committed
// but whose result never reached the caller still tombstoned its bundles, and
// the reaper finds them through RetiredBundles, read as goobers_controller.
func TestLostReapResultIsRecoveredFromTombstones(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	r := record("run-lost")
	if _, err := store.Publish(t.Context(), handle, r, "bundles/run-lost", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := store.Reap(t.Context(), handle, 10); err != nil {
		t.Fatal(err)
	}
	// The reap result is discarded, as a caller that lost the reply would.
	config := pool.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	controller, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	var role string
	if err := controller.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("controller pool runs as %q (%v)", role, err)
	}
	reader := recoverypg.Store{Pool: controller, Fence: manager}
	retired, err := reader.RetiredBundles(t.Context(), handle.InstanceID, 10)
	if err != nil || len(retired) != 1 || retired[0] != "bundles/run-lost" {
		t.Fatalf("retired bundles = %v, %v; want the lost reap's bundle", retired, err)
	}
	for range 2 {
		// The second release is the retry after an ambiguous first: a no-op.
		if err := store.ReleaseBundle(t.Context(), handle, "bundles/run-lost"); err != nil {
			t.Fatal(err)
		}
	}
	if left, err := reader.RetiredBundles(t.Context(), handle.InstanceID, 10); err != nil || len(left) != 0 {
		t.Fatalf("after release, retired bundles = %v, %v; want none", left, err)
	}
}

// reapOne publishes run's record at bundle with a short retention and reaps
// it, discarding the result.
func reapOne(t *testing.T, store recoverypg.Store, handle *lease.Handle, run, bundle string) {
	t.Helper()
	if _, err := store.Publish(t.Context(), handle, record(run), bundle, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := store.Reap(t.Context(), handle, 10); err != nil {
		t.Fatal(err)
	}
}

// TestRetiredBundlesListOnlyTheInstancesOwn: on a shared database another
// instance's tombstones are never listed; this instance could never release
// them, and deleting them would cross tenants.
func TestRetiredBundlesListOnlyTheInstancesOwn(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	managerA := pgtest.Manager(t, pool, "holder", 0)
	handleA := pgtest.Acquire(t, managerA)
	managerB, err := lease.New(lease.Config{Pool: pool, InstanceID: "other-instance", Name: "control", Holder: "holder-b"})
	if err != nil {
		t.Fatal(err)
	}
	handleB := pgtest.Acquire(t, managerB)
	storeA, storeB := recoverypg.Store{Pool: pool, Fence: managerA}, recoverypg.Store{Pool: pool, Fence: managerB}
	reapOne(t, storeA, handleA, "run-a", "bundles/a")
	reapOne(t, storeB, handleB, "run-b", "bundles/b")
	for _, tc := range []struct {
		store  recoverypg.Store
		handle *lease.Handle
		want   string
	}{{storeA, handleA, "bundles/a"}, {storeB, handleB, "bundles/b"}} {
		got, err := tc.store.RetiredBundles(t.Context(), tc.handle.InstanceID, 10)
		if err != nil || len(got) != 1 || got[0] != tc.want {
			t.Fatalf("instance %s retired bundles = %v, %v; want only its own %q", tc.handle.InstanceID, got, err, tc.want)
		}
	}
}

// TestRetiredBundlesHonourTheLimitOldestReapedFirst: the list is bounded and
// ordered by when each bundle was reaped, not by key, so a bundle that can
// never be deleted at the head of the key order cannot starve the rest.
func TestRetiredBundlesHonourTheLimitOldestReapedFirst(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	// Key order is the reverse of reap order.
	for i, bundle := range []string{"bundles/z", "bundles/y", "bundles/x"} {
		reapOne(t, store, handle, fmt.Sprintf("run-%d", i), bundle)
	}
	got, err := store.RetiredBundles(t.Context(), handle.InstanceID, 2)
	if err != nil || !slices.Equal(got, []string{"bundles/z", "bundles/y"}) {
		t.Fatalf("RetiredBundles(2) = %v, %v; want the two oldest reaped [bundles/z bundles/y]", got, err)
	}
	for _, limit := range []int{0, -1} {
		if got, err := store.RetiredBundles(t.Context(), handle.InstanceID, limit); err != nil || got != nil {
			t.Fatalf("RetiredBundles(%d) = %v, %v; want nothing", limit, got, err)
		}
	}
}

// TestReapPublishRaceOnSharedBundle is reviewer-3's D2 probe, kept as a
// regression test. Publish checks the tombstone, then inserts. If a
// concurrent Reap has deleted the record holding the same bundle but not yet
// committed, Publish's tombstone read misses the uncommitted tombstone, its
// INSERT waits on the unique index, and once Reap commits the INSERT succeeds:
// a live record now references a bundle Reap handed back for deletion.
// Deterministic: a test-only trigger parks Reap's tombstone INSERT on an
// advisory lock; pg_locks confirms both transactions are waiting.
func TestReapPublishRaceOnSharedBundle(t *testing.T) {
	pool := pgtest.Pool(t, "recoverypg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := recoverypg.Store{Pool: pool, Fence: manager}
	expiring, other := record("expiring"), record("other")
	if _, err := store.Publish(t.Context(), handle, expiring, "bundles/b", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`ALTER TABLE control.recovery_record DISABLE TRIGGER recovery_record_guard`,
		`UPDATE control.recovery_record SET retain_until=now()-interval '1 second'`,
		`ALTER TABLE control.recovery_record ENABLE TRIGGER recovery_record_guard`,
		`CREATE FUNCTION control.race_park() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN PERFORM pg_advisory_xact_lock(78660); RETURN NEW; END $$`,
		`CREATE TRIGGER race_park BEFORE INSERT ON control.recovery_bundle_tombstone
		 FOR EACH ROW EXECUTE FUNCTION control.race_park()`,
	} {
		if _, err := pool.Exec(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	holder, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock(78660)`); err != nil {
		t.Fatal(err)
	}
	reapDone := make(chan error, 1)
	go func() {
		reaped, err := store.Reap(context.Background(), handle, 10)
		if err == nil && len(reaped) != 1 {
			err = fmt.Errorf("reaped %d", len(reaped))
		}
		reapDone <- err
	}()
	waitFor(t, pool, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=78660 AND NOT granted`, 1)
	publishDone := make(chan error, 1)
	go func() {
		_, err := store.Publish(context.Background(), handle, other, "bundles/b", 48*time.Hour)
		publishDone <- err
	}()
	// Publish is parked on the unique index behind Reap's uncommitted delete.
	waitFor(t, pool, `SELECT count(*) FROM pg_locks WHERE locktype='transactionid' AND NOT granted`, 1)
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_unlock(78660)`); err != nil {
		t.Fatal(err)
	}
	if err := <-reapDone; err != nil {
		t.Fatal(err)
	}
	publishErr := <-publishDone
	var live, tomb int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM control.recovery_record WHERE bundle_key='bundles/b'),
		(SELECT count(*) FROM control.recovery_bundle_tombstone WHERE bundle_key='bundles/b')`).Scan(&live, &tomb); err != nil {
		t.Fatal(err)
	}
	if publishErr == nil && live == 1 && tomb == 1 {
		t.Fatalf("Publish succeeded with bundles/b while Reap handed it back for deletion (live=%d tombstoned=%d)", live, tomb)
	}
	if publishErr != nil && !errors.Is(publishErr, recovery.ErrBundleUnavailable) {
		t.Fatalf("publish err = %v", publishErr)
	}
}

// waitFor polls a catalog count until it reaches want (a barrier on observed
// lock state, not a timing assumption); it fails after 10s.
func waitFor(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, query string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(t.Context(), query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("barrier %q never reached %d (at %d)", query, want, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
