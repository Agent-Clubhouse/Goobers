//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/migrate"
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
		if err := store.RecordDispatch(t.Context(), record.ID, "run-1"); err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(t.Context(), record.ID, triggerqueue.Dispatched, "run-1", "", now.Add(time.Minute)); err != nil {
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
	second, err := New(pool, "test-instance", 2)
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
	if err := second.BeginDispatch(t.Context(), liveRecord.ID); err != nil {
		t.Fatal(err)
	}
	uncertain, err := first.Uncertain(t.Context(), []int64{2}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(uncertain) != 1 || uncertain[0].ID != oldRecord.ID {
		t.Fatalf("uncertain = %+v, want only old epoch", uncertain)
	}
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
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "acceptance-test",
		InstanceID: "migration", Holder: "migration",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	store, err := New(pool, "test-instance", epoch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return store, pool
}
