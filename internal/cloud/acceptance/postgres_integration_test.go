//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestSchedulerPolicyStoreSuite(t *testing.T) {
	t.Run("pg", func(t *testing.T) {
		store, _ := testStore(t, 1)
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		record, repeated, err := store.Accept(t.Context(), "manual/1", "operator", []byte(`{"request":1}`), now)
		if err != nil || repeated {
			t.Fatalf("accept = %+v, %v, %v", record, repeated, err)
		}
		replay, repeated, err := store.Accept(t.Context(), "manual/1", "operator", []byte(`{"request":1}`), now.Add(time.Minute))
		if err != nil || !repeated || replay.ID != record.ID {
			t.Fatalf("replay = %+v, %v, %v", replay, repeated, err)
		}
		if err := store.BeginDispatch(t.Context(), record.ID); err != nil {
			t.Fatal(err)
		}
		runID := strings.TrimPrefix(record.ID, "trigger-")
		if err := store.RecordDispatch(t.Context(), record.ID, runID); err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(t.Context(), record.ID, triggerqueue.Dispatched, runID, "", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAcceptanceKeyReuseAfterRetentionStartsNewRun(t *testing.T) {
	store, _ := testStore(t, 1)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	first, _, err := store.Accept(t.Context(), "manual/key", "operator", []byte("payload"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatch(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(t.Context(), first.ID, triggerqueue.Rejected, "", "rejected", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	second, repeated, err := store.Accept(
		t.Context(), "manual/key", "operator", []byte("payload"),
		now.Add(triggerqueue.ReplayRetention+time.Second),
	)
	if err != nil || repeated || second.ID == first.ID {
		t.Fatalf("post-retention accept = %+v repeated=%v err=%v", second, repeated, err)
	}
}

func TestSourceCursorCASRejectsStaleBefore(t *testing.T) {
	store, _ := testStore(t, 1)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := store.SourceCursorRevision(t.Context(), "scope", "revision", base, base, false); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.AcceptSource(t.Context(), triggerqueue.SourceBatch{
		Key: "source/stale", Actor: "scheduler", Fingerprint: "stale",
		Advance: &triggerqueue.SourceAdvance{
			Scope: "scope", Revision: "revision",
			Before: base.Add(time.Minute), After: base.Add(2 * time.Minute),
		},
	}, base.Add(time.Minute))
	if !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("stale cursor error = %v, want ErrTransition", err)
	}
	cursor, _, err := store.SourceCursorRevision(t.Context(), "scope", "revision", base, base, false)
	if err != nil || !cursor.Equal(base) {
		t.Fatalf("cursor = %v, %v", cursor, err)
	}
}

func TestUncertainExcludesLiveEpoch(t *testing.T) {
	first, pool := testStore(t, 1)
	second, err := New(pool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	oldRecord, _, err := first.Accept(t.Context(), "old", "operator", []byte("old"), now)
	if err != nil {
		t.Fatal(err)
	}
	liveRecord, _, err := second.Accept(t.Context(), "live", "operator", []byte("live"), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.BeginDispatch(t.Context(), oldRecord.ID); err != nil {
		t.Fatal(err)
	}
	if err := first.manager.Release(t.Context(), first.handle); err != nil {
		t.Fatal(err)
	}
	successor, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "test-instance", Name: "control", Holder: "successor",
	})
	if err != nil {
		t.Fatal(err)
	}
	successorHandle, err := successor.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second.BindFence(successor, successorHandle)
	if err := second.BeginDispatch(t.Context(), liveRecord.ID); err != nil {
		t.Fatal(err)
	}
	uncertain, err := first.Uncertain(t.Context(), []int64{successorHandle.Epoch}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(uncertain) != 1 || uncertain[0].ID != oldRecord.ID {
		t.Fatalf("uncertain = %+v, want only old epoch", uncertain)
	}
}

func TestStaleControllerCannotClaimOrFinish(t *testing.T) {
	old, pool := testStore(t, 1)
	now := time.Now().UTC()
	stolen, _, err := old.Accept(t.Context(), "stolen", "operator", []byte("payload"), now)
	if err != nil {
		t.Fatal(err)
	}
	owned, _, err := old.Accept(t.Context(), "owned", "operator", []byte("payload"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	if err := current.BeginDispatch(t.Context(), owned.ID); err != nil {
		t.Fatal(err)
	}
	if err := old.BeginDispatch(t.Context(), stolen.ID); err == nil {
		t.Fatal("stale controller claimed an accepted row")
	}
	if err := old.Finish(
		t.Context(), owned.ID, triggerqueue.Dispatched,
		strings.TrimPrefix(owned.ID, "trigger-"), "", now,
	); err == nil {
		t.Fatal("stale controller finished a successor-owned row")
	}
}

func TestCursorCannotRewindOrRestoreStaleRevision(t *testing.T) {
	old, pool := testStore(t, 1)
	base := time.Now().UTC().Truncate(time.Second)
	if _, _, err := old.SourceCursorRevision(t.Context(), "scope", "A", base, base, false); err != nil {
		t.Fatal(err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	future := base.Add(time.Hour)
	if _, _, err := current.SourceCursorRevision(t.Context(), "scope", "B", base, future, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.SourceCursorRevision(t.Context(), "scope", "A", base, base.Add(time.Minute), false); err == nil {
		t.Fatal("stale controller restored an old revision")
	}
	if _, _, err := current.SourceCursorRevision(t.Context(), "scope", "C", base, base.Add(time.Minute), false); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("backward revision error = %v, want ErrTransition", err)
	}
}

func TestCoalescedDemandCommitsCursor(t *testing.T) {
	store, _ := testStore(t, 1)
	base := time.Now().UTC().Truncate(time.Second)
	if _, _, err := store.SourceCursorRevision(t.Context(), "scope", "A", base, base, false); err != nil {
		t.Fatal(err)
	}
	first := triggerqueue.ScheduleDemandCapture{
		ID: "first", Payload: []byte("payload"), AcceptedAt: base.Add(time.Minute),
		Advance: triggerqueue.SourceAdvance{
			Scope: "scope", Revision: "A", Before: base, After: base.Add(time.Minute),
		},
	}
	if err := store.CaptureScheduleDemand(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = "second"
	second.Advance.Before = first.Advance.After
	second.Advance.After = base.Add(2 * time.Minute)
	if err := store.CaptureScheduleDemand(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	cursor, _, err := store.SourceCursorRevision(t.Context(), "scope", "A", base, base, false)
	if err != nil || !cursor.Equal(second.Advance.After) {
		t.Fatalf("coalesced cursor = %v, %v", cursor, err)
	}
}

func TestConcurrentAcceptanceIsIdempotent(t *testing.T) {
	store, _ := testStore(t, 1)
	const contenders = 12
	results := make(chan triggerqueue.Record, contenders)
	errorsCh := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, _, err := store.Accept(
				t.Context(), "concurrent", "operator", []byte("same"), time.Now().UTC(),
			)
			results <- record
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	var id string
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent accept: %v", err)
		}
	}
	for record := range results {
		if id == "" {
			id = record.ID
		}
		if record.ID != id {
			t.Fatalf("concurrent IDs differ: %s and %s", id, record.ID)
		}
	}
}

func TestUncertainWithNoLiveEpochsIncludesAll(t *testing.T) {
	store, _ := testStore(t, 1)
	record, _, err := store.Accept(t.Context(), "uncertain", "operator", []byte("payload"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	uncertain, err := store.Uncertain(t.Context(), nil, 10)
	if err != nil || len(uncertain) != 1 || uncertain[0].ID != record.ID {
		t.Fatalf("uncertain = %+v, %v", uncertain, err)
	}
}

func TestRecordDispatchMustUseAssignedID(t *testing.T) {
	store, _ := testStore(t, 1)
	record, _, err := store.Accept(t.Context(), "assigned", "operator", []byte("payload"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDispatch(t.Context(), record.ID, "invented"); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("invented run ID error = %v", err)
	}
	assigned := strings.TrimPrefix(record.ID, "trigger-")
	if err := store.RecordDispatch(t.Context(), record.ID, assigned); err != nil {
		t.Fatal(err)
	}
}

func successorStore(t *testing.T, pool *pgxpool.Pool) *Store {
	t.Helper()
	store, err := New(pool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "test-instance", Name: "control", Holder: "successor",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	return store
}

func testStore(t *testing.T, epoch int64) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := "acceptance_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, pool)
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "acceptance-test",
		InstanceID: "migration", Holder: "migration",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	store, err := New(pool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "test-instance", Name: "control",
		Holder: "controller-" + fmt.Sprint(epoch),
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return store, pool
}
