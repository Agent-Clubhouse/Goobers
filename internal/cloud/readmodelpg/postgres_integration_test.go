//go:build cloudintegration

package readmodelpg_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	cloudschema "github.com/goobers/goobers/internal/cloud/schema"
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
	admin := testPool(t)
	applyMigrations(t, admin)
	resetReadmodel(t, admin)
	pool := controllerPool(t, admin)
	var version atomic.Int64
	version.Store(1)
	conformance.Run(t, func(t *testing.T) readmodel.Backend {
		current := int(version.Add(1))
		fence := testFence{pool: pool, epoch: int64(current)}
		builder, err := readmodelpg.NewBuilder(
			admin, testFence{pool: admin, epoch: int64(current)})
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.CreateEmptyVersion(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		reconcileReadmodelGrants(t, admin)
		activateVersion(t, admin, current, int64(current))
		store, err := readmodelpg.NewVersion(t.Context(), pool, current, fence)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			schema := pgx.Identifier{fmt.Sprintf("readmodel_v%d", current)}.Sanitize()
			if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
				t.Errorf("drop test schema: %v", err)
			}
		})
		return store
	})
}

func TestChangeFeedAsControllerRole(t *testing.T) {
	admin := testPool(t)
	applyMigrations(t, admin)
	resetReadmodel(t, admin)
	controller := controllerPool(t, admin)
	store, err := readmodelpg.New(
		t.Context(), controller, testFence{pool: controller, epoch: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, second := store.ChangeFeed(), store.ChangeFeed()
	if first != second {
		t.Fatalf("ChangeFeed identity changed: %p != %p", first, second)
	}
	state, err := store.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishDefinitionsChanged(t.Context()); err != nil {
		t.Fatal(err)
	}
	position, err := first.Since(t.Context(), readmodel.Cursor{
		SchemaVersion: state.SchemaVersion,
		Epoch:         state.Epoch,
		Seq:           state.MinChangeSeq,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(position.Changes) != 1 ||
		position.Changes[0].Kind != readmodel.ChangeDefinitionsChanged {
		t.Fatalf("definitions changes = %+v", position.Changes)
	}
}

func TestRetiredWriterRejectsPostFlipWrite(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	retained, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := retained.UpsertRun(t.Context(), testProjection("retained", 1)); err != nil {
		t.Fatal(err)
	}
	builder, _ := readmodelpg.NewBuilder(pool, fence)
	if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
		t.Fatal(err)
	}
	if err := builder.Flip(t.Context(), "readmodel", 2); err != nil {
		t.Fatal(err)
	}
	if err := retained.UpsertRun(t.Context(), testProjection("retained", 2)); !errors.Is(err, readmodelpg.ErrVersionMismatch) {
		t.Fatalf("retired write = %v, want ErrVersionMismatch", err)
	}
	active, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := active.GetRun(t.Context(), "retained")
	if err != nil || !ok || got.LastSeq != 1 {
		t.Fatalf("active row after rejected write = %+v %v %v", got, ok, err)
	}
}

func TestRebuildCatchesWritesBeforeFlip(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	source, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertRun(t.Context(), testProjection("catch-up", 1)); err != nil {
		t.Fatal(err)
	}
	builder, _ := readmodelpg.NewBuilder(pool, fence)
	if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertRun(t.Context(), testProjection("catch-up", 2)); err != nil {
		t.Fatal(err)
	}
	if err := builder.Flip(t.Context(), "readmodel", 2); err == nil {
		t.Fatal("flip accepted a target that missed a post-build write")
	}
	if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
		t.Fatal(err)
	}
	if err := builder.Flip(t.Context(), "readmodel", 2); err != nil {
		t.Fatal(err)
	}
	active, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := active.GetRun(t.Context(), "catch-up")
	if err != nil || !ok || got.LastSeq != 2 {
		t.Fatalf("caught-up row = %+v %v %v", got, ok, err)
	}
}

func TestCreateEmptyVersionCannotDropNewlyActiveSchema(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	builder, _ := readmodelpg.NewBuilder(pool, fence)
	if err := builder.CreateEmptyVersion(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE TABLE readmodel_v2.marker(value integer)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(t.Context()) }()
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE readmodel_v2.meta IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(t.Context(), `
		SELECT active_version FROM control.readmodel_version
		WHERE name='readmodel' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- builder.CreateEmptyVersion(context.Background(), 2) }()
	waitForRebuildLock(t, pool)
	if _, err := blocker.Exec(t.Context(), `
		UPDATE control.readmodel_version SET active_version=2
		WHERE name='readmodel'`); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "refusing to replace active") {
		t.Fatalf("CreateEmptyVersion after flip = %v", err)
	}
	var markerExists bool
	if err := pool.QueryRow(t.Context(),
		`SELECT to_regclass('readmodel_v2.marker') IS NOT NULL`).Scan(&markerExists); err != nil {
		t.Fatal(err)
	}
	if !markerExists {
		t.Fatal("CreateEmptyVersion dropped the newly active schema")
	}
}

func TestStageFiltersMatchSQLite(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	pgStore, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	sqlite, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlite.Close() }()
	projections := []readmodel.Projection{
		stageProjection("success", readmodel.StageRow{
			Stage: "build", HadSuccess: true, TokenMeasured: true,
		}),
		stageProjection("failure", readmodel.StageRow{
			Stage: "build", HadFailure: true, CostMeasured: true,
		}),
		stageProjection("other", readmodel.StageRow{
			Stage: "test", HadOther: true, RetryWaste: true,
		}),
	}
	for _, projection := range projections {
		if err := sqlite.UpsertRun(t.Context(), projection); err != nil {
			t.Fatal(err)
		}
		if err := pgStore.UpsertRun(t.Context(), projection); err != nil {
			t.Fatal(err)
		}
	}
	cases := []readmodel.ListOptions{
		{Stage: "build", Outcome: readmodel.OutcomeSuccess},
		{Stage: "build", Outcome: readmodel.OutcomeFailure},
		{Stage: "build", Outcome: readmodel.OutcomeTerminal},
		{Stage: "build", Outcome: readmodel.OutcomeFinished},
		{Stage: "build", Population: readmodel.PopulationTokenMeasured},
		{Population: readmodel.PopulationCostMeasured},
	}
	for _, options := range cases {
		options.Limit = 20
		want, err := sqlite.ListRuns(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgStore.ListRuns(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(runIDs(got), runIDs(want)) {
			t.Fatalf("options %+v: PG=%v SQLite=%v", options, runIDs(got), runIDs(want))
		}
	}
}

func TestConcurrentFirstProjectionIsIdempotent(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	store, err := readmodelpg.New(t.Context(), pool, testFence{pool: pool, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	projection := testProjection("concurrent-first", 1)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- store.UpsertRun(context.Background(), projection)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay: %v", err)
		}
	}
	changes, err := store.Changes(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != readmodel.ChangeRunCreated {
		t.Fatalf("changes = %+v, want one run.created", changes)
	}
}

func TestConcurrentFirstProjectionNeverDuplicatesCreated(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	store, err := readmodelpg.New(t.Context(), pool, testFence{pool: pool, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, seq := range []uint64{1, 2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- store.UpsertRun(context.Background(), testProjection("concurrent-advance", seq))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent projection: %v", err)
		}
	}
	changes, err := store.Changes(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, change := range changes {
		if change.Kind == readmodel.ChangeRunCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("changes = %+v, want exactly one run.created", changes)
	}
	got, ok, err := store.GetRun(t.Context(), "concurrent-advance")
	if err != nil || !ok || got.LastSeq != 2 {
		t.Fatalf("final row = %+v %v %v", got, ok, err)
	}
}

func TestRebuildUsesResumableFencedBatches(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	if _, err := pool.Exec(t.Context(), `DROP SCHEMA IF EXISTS readmodel_v3 CASCADE`); err != nil {
		t.Fatal(err)
	}
	seedRebuildRows(t, pool, 250001)
	base := testFence{pool: pool, epoch: 7}
	failing := &countingFence{base: base, failAt: 3}
	builder, _ := readmodelpg.NewBuilder(pool, failing)
	if err := builder.Build(t.Context(), "readmodel_v3"); !errors.Is(err, errInjectedFence) {
		t.Fatalf("interrupted build = %v, want injected fence error", err)
	}
	resumed := &countingFence{base: base}
	builder, _ = readmodelpg.NewBuilder(pool, resumed)
	if err := builder.Build(t.Context(), "readmodel_v3"); err != nil {
		t.Fatal(err)
	}
	if resumed.calls.Load() < 2500 {
		t.Fatalf("resumed rebuild used %d fenced txs, want multiple batches", resumed.calls.Load())
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM readmodel_v3.run`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 250001 {
		t.Fatalf("rebuilt rows = %d, want 250001", count)
	}
}

func TestLeaseFenceAdapterUsesRealHandleEpoch(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "readmodel-test", Name: "projector", Holder: "holder-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := readmodelpg.NewLeaseFence(manager, handle)
	if err != nil {
		t.Fatal(err)
	}
	store, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(t.Context(), testProjection("lease-adapter", 1)); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := pool.QueryRow(t.Context(), `
		SELECT owner_epoch FROM readmodel_v1.run WHERE run_id='lease-adapter'`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != handle.Epoch {
		t.Fatalf("row epoch = %d, handle epoch = %d", epoch, handle.Epoch)
	}
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

func TestRebuildPreservesSafeFeedFloor(t *testing.T) {
	admin := testPool(t)
	applyMigrations(t, admin)
	resetReadmodel(t, admin)
	pool := controllerPool(t, admin)
	fence := testFence{pool: pool, epoch: 7}
	source, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertRun(t.Context(), testProjection("baseline", 1)); err != nil {
		t.Fatal(err)
	}
	builder, _ := readmodelpg.NewBuilder(
		admin, testFence{pool: admin, epoch: 7})
	if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
		t.Fatal(err)
	}
	reconcileReadmodelGrants(t, admin)
	if err := builder.Flip(t.Context(), "readmodel", 2); err != nil {
		t.Fatal(err)
	}
	active, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	state, err := active.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	latest, err := active.LatestChangeSeq(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	changes, err := active.Changes(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if latest != 1 || state.MinChangeSeq != 2 || len(changes) != 0 {
		t.Fatalf("rebuilt feed: latest=%d floor=%d changes=%v, want 1/2/empty",
			latest, state.MinChangeSeq, changes)
	}
	head, err := active.ChangeFeed().Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head.Seq != state.MinChangeSeq-1 {
		t.Fatalf("rebuilt head seq = %d, want floor-1 = %d",
			head.Seq, state.MinChangeSeq-1)
	}
	condition := readmodel.EvaluateCursor(readmodel.Cursor{
		SchemaVersion: state.SchemaVersion, Epoch: state.Epoch, Seq: latest,
	}, state)
	if condition != readmodel.CursorOK {
		t.Fatalf("resume immediately before floor = %q, want %q",
			condition, readmodel.CursorOK)
	}
	if err := active.PublishDefinitionsChanged(t.Context()); err != nil {
		t.Fatal(err)
	}
	position, err := active.ChangeFeed().Since(t.Context(), head, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(position.Changes) != 1 ||
		position.Changes[0].Kind != readmodel.ChangeDefinitionsChanged {
		t.Fatalf("first post-flip change = %+v", position.Changes)
	}
}

func TestStaleOwnerRejectedAfterTakeover(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	version := 2
	newFence := testFence{pool: pool, epoch: 12}
	builder, err := readmodelpg.NewBuilder(pool, newFence)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.CreateEmptyVersion(t.Context(), version); err != nil {
		t.Fatal(err)
	}
	activateVersion(t, pool, version, 11)
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
	// A private database per test: migrate's grant reconcile spans every
	// schema in the database, so tests must not share one.
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := "readmodelpg_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 63 {
		name = name[:63]
	}
	identifier := pgx.Identifier{name}.Sanitize()
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
	config.ConnConfig.Database = name
	config.MaxConns = cloudlease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return pool
}

func controllerPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func reconcileReadmodelGrants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	inventory, err := cloudschema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := cloudschema.ReconcileGrants(t.Context(), tx, inventory); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, pool)
	runner := migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "readmodel-integration-pitr",
		InstanceID: "readmodel-migration", Holder: "readmodel-migration",
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func resetReadmodel(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
		DROP SCHEMA IF EXISTS readmodel_v2 CASCADE;
		DELETE FROM readmodel_v1.run_stage;
		DELETE FROM readmodel_v1.change;
		DELETE FROM readmodel_v1.source_watermark;
		DELETE FROM readmodel_v1.run;
		UPDATE readmodel_v1.meta
		SET min_change_seq=0,next_change_seq=0,ready=true,source_version=NULL,
		    run_cursor='',base_complete=true,catchup_seq=0,owner_epoch=1;
		INSERT INTO control.readmodel_version (name, active_version, owner_epoch)
		VALUES ('readmodel', 1, 1)
		ON CONFLICT (name) DO UPDATE
		SET active_version = 1, owner_epoch = 1`)
	if err != nil {
		t.Fatal(err)
	}
}

func activateVersion(t *testing.T, pool *pgxpool.Pool, version int, epoch int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
			UPDATE control.readmodel_version
			SET active_version=$1,owner_epoch=$2,updated_at=clock_timestamp()
			WHERE name='readmodel'`, version, epoch); err != nil {
		t.Fatal(err)
	}
}

func stageProjection(runID string, stage readmodel.StageRow) readmodel.Projection {
	projection := testProjection(runID, 1)
	stage.RunID = runID
	projection.Stages = []readmodel.StageRow{stage}
	projection.Run.Stages = []string{stage.Stage}
	projection.Run.AnyTokenMeasured = stage.TokenMeasured
	projection.Run.AnyPremiumMeasured = stage.PremiumMeasured
	projection.Run.AnyCostMeasured = stage.CostMeasured
	projection.Run.AnyRetryWaste = stage.RetryWaste
	return projection
}

func runIDs(page readmodel.ListPage) []string {
	result := make([]string, len(page.Runs))
	for index, run := range page.Runs {
		result[index] = run.RunID
	}
	return result
}

var errInjectedFence = errors.New("injected fence interruption")

type countingFence struct {
	base   testFence
	failAt int32
	calls  atomic.Int32
}

func (f *countingFence) WithFence(
	ctx context.Context,
	fn func(context.Context, pgx.Tx, int64) error,
) error {
	call := f.calls.Add(1)
	if f.failAt > 0 && call == f.failAt {
		return errInjectedFence
	}
	bounded, cancel := context.WithTimeout(ctx, cloudlease.FencedTransactionTimeout)
	defer cancel()
	return f.base.WithFence(bounded, fn)
}

func seedRebuildRows(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
			INSERT INTO readmodel_v1.run (
				run_id,source_seq,projection,gaggle,workflow,phase,terminal,
				started_at,last_activity_at,disposition,stages,
				any_token_measured,any_premium_measured,any_cost_measured,
				any_retry_waste,owner_epoch
			)
			SELECT lpad(n::text,32,'0'),1,
			       jsonb_build_object('Run',jsonb_build_object('RunID',lpad(n::text,32,'0'))),
			       'g','w','completed',true,clock_timestamp(),clock_timestamp(),'produced',
			       ARRAY[]::text[],false,false,false,false,1
			FROM generate_series(1,$1) AS n`, count); err != nil {
		t.Fatal(err)
	}
}

func waitForRebuildLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(t.Context(), `
				SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE datname=current_database() AND wait_event_type='Lock'
					  AND (
					    query LIKE '%SELECT active_version FROM control.readmodel_version%'
					    OR query LIKE 'DROP SCHEMA%'
					  )
				)`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CreateEmptyVersion did not block on the pointer or target schema")
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

func rolePool(t *testing.T, admin *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestActiveReaderFollowsAFlipForBothRuntimeRoles is the #7865 wiring
// condition: Build, then the grant reconcile, then Flip, so a newly active
// readmodel_vN is usable by goobers_api and goobers_controller the moment it
// becomes active. The control (reconcile skipped) proves the test would see the
// window the ordering exists to close.
func TestActiveReaderFollowsAFlipForBothRuntimeRoles(t *testing.T) {
	for name, reconcile := range map[string]bool{"reconcile before flip": true, "reconcile skipped": false} {
		t.Run(name, func(t *testing.T) {
			admin := testPool(t)
			applyMigrations(t, admin)
			resetReadmodel(t, admin)
			reconcileReadmodelGrants(t, admin)
			fence := testFence{pool: admin, epoch: 7}
			store, err := readmodelpg.New(t.Context(), admin, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertRun(t.Context(), testProjection("run", 1)); err != nil {
				t.Fatal(err)
			}
			readers := map[string]*readmodelpg.ActiveReader{}
			for role, pool := range map[string]*pgxpool.Pool{
				"goobers_api": rolePool(t, admin, "goobers_api"), "goobers_controller": rolePool(t, admin, "goobers_controller"),
			} {
				reader, err := readmodelpg.NewActiveReader(t.Context(), pool)
				if err != nil {
					t.Fatalf("%s cannot open v1: %v", role, err)
				}
				if _, ok, err := reader.GetRun(t.Context(), "run"); err != nil || !ok {
					t.Fatalf("%s reads v1: ok=%v err=%v", role, ok, err)
				}
				readers[role] = reader
			}
			builder, err := readmodelpg.NewBuilder(admin, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
				t.Fatal(err)
			}
			if reconcile {
				reconcileReadmodelGrants(t, admin)
			}
			if err := builder.Flip(t.Context(), "readmodel", 2); err != nil {
				t.Fatal(err)
			}
			for role, reader := range readers {
				_, ok, err := reader.GetRun(t.Context(), "run")
				var refusal *pgconn.PgError
				switch {
				case reconcile && (err != nil || !ok):
					t.Fatalf("%s cannot use the new version right after the flip: ok=%v err=%v", role, ok, err)
				case !reconcile && (!errors.As(err, &refusal) || refusal.Code != "42501"):
					t.Fatalf("%s without the reconcile = ok=%v err=%v, want 42501 (the grant window)", role, ok, err)
				}
			}
			if reconcile {
				for role, reader := range readers {
					state, err := reader.State(t.Context())
					if err != nil || !state.Ready {
						t.Fatalf("%s state after flip = %+v %v", role, state, err)
					}
				}
			}
		})
	}
}

func TestFreshnessReporterOnTheActiveModelForBothRuntimeRoles(t *testing.T) {
	admin := testPool(t)
	applyMigrations(t, admin)
	resetReadmodel(t, admin)
	reconcileReadmodelGrants(t, admin)
	store, err := readmodelpg.New(t.Context(), admin, testFence{pool: admin, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(t.Context(), testProjection("run", 3)); err != nil {
		t.Fatal(err)
	}
	// No recorded pass: nothing says a projector is alive, so the model must
	// not read as current (a dead projector cannot report itself dead).
	first, err := readmodelpg.NewActiveReader(t.Context(), rolePool(t, admin, "goobers_api"))
	if err != nil {
		t.Fatal(err)
	}
	if unproven, err := first.ReadState(t.Context(), readmodel.ReadStateInput{}); err != nil || !reflect.DeepEqual(unproven.Degraded, []string{readmodel.DegradedNoProjectorPass}) {
		t.Fatalf("no recorded pass read as %+v (%v), want no_projector_pass", unproven.Degraded, err)
	}
	if err := store.RecordPass(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	for role, pool := range map[string]*pgxpool.Pool{"goobers_api": rolePool(t, admin, "goobers_api"), "goobers_controller": rolePool(t, admin, "goobers_controller")} {
		reader, err := readmodelpg.NewActiveReader(t.Context(), pool)
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := reader.SourceApplied(t.Context(), "absent"); err != nil || found {
			t.Fatalf("%s: unprojected run found=%v err=%v", role, found, err)
		}
		position, found, err := reader.SourceApplied(t.Context(), "run")
		if err != nil || !found || position.JournalSeq != 3 {
			t.Fatalf("%s: SourceApplied = %+v %v %v", role, position, found, err)
		}
		for required, want := range map[uint64]bool{2: true, 3: true, 4: false} {
			got, err := reader.SatisfiesSourceApplied(t.Context(), readmodel.SourcePosition{RunID: "run", JournalSeq: required})
			if err != nil || got != want {
				t.Fatalf("%s: SatisfiesSourceApplied(%d) = %v %v, want %v", role, required, got, err, want)
			}
		}
		if got, err := reader.SatisfiesSourceApplied(t.Context(), readmodel.SourcePosition{RunID: "absent", JournalSeq: 1}); err != nil || got {
			t.Fatalf("%s: an unprojected run satisfied a position: %v %v", role, got, err)
		}
		healthy, err := reader.ReadState(t.Context(), readmodel.ReadStateInput{})
		if err != nil {
			t.Fatal(err)
		}
		state, _ := reader.State(t.Context())
		latest, _ := reader.LatestChangeSeq(t.Context())
		if healthy.Epoch != state.Epoch || healthy.AppliedSeq != latest || healthy.Completeness != readmodel.CompletenessComplete ||
			len(healthy.Degraded) != 0 || healthy.LastSweepCompletedAt != nil || healthy.ObservedAt.IsZero() {
			t.Fatalf("%s: healthy read state = %+v (state %+v latest %d)", role, healthy, state, latest)
		}
		degraded, err := reader.ReadState(t.Context(), readmodel.ReadStateInput{PendingIntake: 2, ProjectFailures: 1, OldestPendingAt: time.Now().Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if degraded.Completeness != readmodel.CompletenessPartial || len(degraded.Missing) != 1 || degraded.Missing[0].Name != readmodel.MissingProjectedRuns ||
			degraded.Missing[0].ExpectedBy.IsZero() || degraded.LagSeconds < 59 ||
			!reflect.DeepEqual(degraded.Degraded, []string{readmodel.DegradedProjectionLag, readmodel.DegradedProjectFailure}) {
			t.Fatalf("%s: degraded read state = %+v", role, degraded)
		}
	}
}

func TestReadStateDegradesOnAStaleProjectorPassAndSurfacesPersistedFailures(t *testing.T) {
	admin := testPool(t)
	applyMigrations(t, admin)
	resetReadmodel(t, admin)
	reconcileReadmodelGrants(t, admin)
	store, err := readmodelpg.New(t.Context(), admin, testFence{pool: admin, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := readmodelpg.NewActiveReader(t.Context(), rolePool(t, admin, "goobers_api"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPass(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	current, err := reader.ReadState(t.Context(), readmodel.ReadStateInput{})
	if err != nil || len(current.Degraded) != 0 || current.Completeness != readmodel.CompletenessComplete {
		t.Fatalf("a fresh pass read as %+v (%v), want current", current, err)
	}

	// The projector dies: its last pass ages on the database clock.
	if _, err := admin.Exec(t.Context(), `UPDATE readmodel_v1.meta SET last_pass_at = clock_timestamp() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	stale, err := reader.ReadState(t.Context(), readmodel.ReadStateInput{})
	if err != nil || !reflect.DeepEqual(stale.Degraded, []string{readmodel.DegradedProjectorStale}) || stale.LagSeconds < 599 {
		t.Fatalf("a 10 minute old pass read as %+v lag %v (%v), want projector_stale with the age as lag", stale.Degraded, stale.LagSeconds, err)
	}

	// Open failures recorded by the projector reach a reader that was told nothing.
	if err := store.RecordPass(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	failing, err := reader.ReadState(t.Context(), readmodel.ReadStateInput{})
	if err != nil || failing.Completeness != readmodel.CompletenessPartial || !reflect.DeepEqual(failing.Degraded, []string{readmodel.DegradedProjectFailure}) {
		t.Fatalf("persisted open failures read as %+v %v (%v), want partial project_failure", failing.Degraded, failing.Completeness, err)
	}
}

func TestRecordPassIsRefusedToTheAPIRoleAndToAStaleOwner(t *testing.T) {
	admin := testPool(t)
	applyMigrations(t, admin)
	resetReadmodel(t, admin)
	reconcileReadmodelGrants(t, admin)
	owner, err := readmodelpg.New(t.Context(), admin, testFence{pool: admin, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.RecordPass(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	older, err := readmodelpg.New(t.Context(), admin, testFence{pool: admin, epoch: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := older.RecordPass(t.Context(), 0); !errors.Is(err, readmodelpg.ErrStaleOwner) {
		t.Fatalf("a deposed owner recorded a pass: %v", err)
	}
	api := rolePool(t, admin, "goobers_api")
	if _, err := api.Exec(t.Context(), `UPDATE readmodel_v1.meta SET last_pass_at = clock_timestamp()`); err == nil {
		t.Fatal("the API role updated the pass record")
	}
}
