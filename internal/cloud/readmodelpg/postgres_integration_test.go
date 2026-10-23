//go:build cloudintegration

package readmodelpg_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/conformance"
)

type testFence struct {
	pool  *pgxpool.Pool
	epoch int64
}

func (f testFence) WithFence(
	ctx context.Context,
	fn func(context.Context, pgx.Tx, int64) error,
) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx, f.epoch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestPostgresConformance(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	var version atomic.Int64
	version.Store(time.Now().UnixNano())
	conformance.Run(t, func(t *testing.T) readmodel.Backend {
		current := int(version.Add(1))
		fence := testFence{pool: pool, epoch: int64(current)}
		builder, err := readmodelpg.NewBuilder(pool, fence)
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.CreateEmptyVersion(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		store, err := readmodelpg.NewVersion(t.Context(), pool, current, fence)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			schema := pgx.Identifier{fmt.Sprintf("readmodel_v%d", current)}.Sanitize()
			if _, err := pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
				t.Errorf("drop test schema: %v", err)
			}
		})
		return store
	})
}

func TestRebuildFlipIsAtomic(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	oldStore, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	projection := testProjection("old", 1)
	if err := oldStore.UpsertRun(t.Context(), projection); err != nil {
		t.Fatal(err)
	}

	builder, err := readmodelpg.NewBuilder(pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE readmodel_v2.meta SET ready = false WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	before, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := before.GetRun(t.Context(), "old"); err != nil || !ok {
		t.Fatalf("active model changed before flip: ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE readmodel_v2.meta SET ready = true WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err := builder.Flip(t.Context(), "readmodel", 2); err != nil {
		t.Fatal(err)
	}
	after, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := after.GetRun(t.Context(), "old"); err != nil || !ok {
		t.Fatalf("rebuilt model lost committed row: ok=%v err=%v", ok, err)
	}
}

func TestStaleOwnerRejectedAfterTakeover(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	version := int(time.Now().UnixNano())
	newFence := testFence{pool: pool, epoch: 12}
	builder, err := readmodelpg.NewBuilder(pool, newFence)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.CreateEmptyVersion(t.Context(), version); err != nil {
		t.Fatal(err)
	}
	newStore, err := readmodelpg.NewVersion(t.Context(), pool, version, newFence)
	if err != nil {
		t.Fatal(err)
	}
	if err := newStore.UpsertRun(t.Context(), testProjection("owned", 2)); err != nil {
		t.Fatal(err)
	}
	staleStore, err := readmodelpg.NewVersion(
		t.Context(), pool, version, testFence{pool: pool, epoch: 11})
	if err != nil {
		t.Fatal(err)
	}
	err = staleStore.UpsertRun(t.Context(), testProjection("owned", 3))
	if !errors.Is(err, readmodelpg.ErrStaleOwner) {
		t.Fatalf("stale write err = %v, want ErrStaleOwner", err)
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "readmodel-integration-pitr",
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func resetReadmodel(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
		DELETE FROM readmodel_v1.change;
		DELETE FROM readmodel_v1.source_watermark;
		DELETE FROM readmodel_v1.run;
		UPDATE readmodel_v1.meta
		SET min_change_seq = 0, next_change_seq = 0, ready = true, owner_epoch = 1;
		INSERT INTO control.readmodel_version (name, active_version, owner_epoch)
		VALUES ('readmodel', 1, 1)
		ON CONFLICT (name) DO UPDATE
		SET active_version = 1, owner_epoch = 1`)
	if err != nil {
		t.Fatal(err)
	}
}

func testProjection(runID string, seq uint64) readmodel.Projection {
	started := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	return readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: started, LastActivity: finished, FinishedAt: &finished,
		LastSeq: seq,
	}}
}
