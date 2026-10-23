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
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
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
	resetReadmodel(t, pool)
	var version atomic.Int64
	version.Store(1)
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
		activateVersion(t, pool, current, int64(current))
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
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	source, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertRun(t.Context(), testProjection("baseline", 1)); err != nil {
		t.Fatal(err)
	}
	builder, _ := readmodelpg.NewBuilder(pool, fence)
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
	condition := readmodel.EvaluateCursor(readmodel.Cursor{
		SchemaVersion: state.SchemaVersion, Epoch: state.Epoch, Seq: latest,
	}, state)
	if condition != readmodel.CursorFeedTruncated {
		t.Fatalf("resume at discarded baseline = %q, want %q",
			condition, readmodel.CursorFeedTruncated)
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
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = cloudlease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
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
