//go:build cloudintegration

package migrate

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgroles"
)

func TestPostgresMigrationApply(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{
		Direct: pool, Catalog: catalog, BackupRef: "test-pitr-mark",
		InstanceID: "test-instance", Holder: "migration-test",
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("idempotent reapply: %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM control.schema_migration").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(catalog) {
		t.Fatalf("recorded migrations = %d, want %d", count, len(catalog))
	}
	status, err := Status(t.Context(), pool, "0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Compatible || status.MinimumCompatibleBinary != "0.6.0" || len(status.Applied) != len(catalog) || len(status.PendingVersions) != 0 {
		t.Fatalf("migration status = %+v", status)
	}
}

func TestPartitionIndexRetryRepairsInvalidAndReapplies(t *testing.T) {
	// A private database: the grant reconcile in other tests spans every
	// schema in the database it migrates.
	pool := privatePool(t)
	if _, err := pool.Exec(t.Context(), `
		DROP TABLE IF EXISTS review_event CASCADE;
		CREATE TABLE review_event (id integer, created_at integer) PARTITION BY RANGE (id);
		CREATE TABLE review_event_1 PARTITION OF review_event FOR VALUES FROM (0) TO (100);
		INSERT INTO review_event VALUES (1, 10), (2, 10);
		CREATE INDEX review_event_parent_idx ON ONLY review_event (created_at)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS review_event CASCADE")
	})
	if _, err := pool.Exec(t.Context(), `
		CREATE UNIQUE INDEX CONCURRENTLY review_event_1_idx
		ON review_event_1 (created_at)`); err == nil {
		t.Fatal("duplicate values unexpectedly produced a valid unique index")
	}
	plan := []PartitionIndex{{Table: "review_event_1", Index: "review_event_1_idx"}}
	if err := CreatePartitionedIndex(t.Context(), pool, "review_event_parent_idx", []string{"created_at"}, plan); err != nil {
		t.Fatalf("repair invalid index: %v", err)
	}
	if err := CreatePartitionedIndex(t.Context(), pool, "review_event_parent_idx", []string{"created_at"}, plan); err != nil {
		t.Fatalf("idempotent reapply: %v", err)
	}
	var valid, attached bool
	if err := pool.QueryRow(t.Context(), `
		SELECT child.indisvalid,
			EXISTS (
				SELECT 1 FROM pg_inherits
				WHERE inhparent='review_event_parent_idx'::regclass
				  AND inhrelid='review_event_1_idx'::regclass
			)
		FROM pg_index AS child
		WHERE child.indexrelid='review_event_1_idx'::regclass`).Scan(&valid, &attached); err != nil {
		t.Fatal(err)
	}
	if !valid || !attached {
		t.Fatalf("repaired index valid=%v attached=%v", valid, attached)
	}
}

// privatePool creates a database private to the test. The grant reconcile
// covers every schema in the database, so it must not share one with tests
// that leave their own tables behind.
func privatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := "migrate_" + strings.ToLower(t.Name())
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
	config.MaxConns = 4
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

// ensureRoles provisions the cluster-wide roles through pgroles, as
// pgtest.EnsureRoles does; this package's tests cannot import pgtest, which
// imports migrate.
func ensureRoles(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := pgroles.ProvisionRoles(t.Context(), conn.Conn(), nil); err != nil {
		t.Fatalf("provision cloud roles: %v", err)
	}
}

// TestMigrationUpgradesLiveInstanceEventTable models the install #7853 left
// behind: the OLD runner (no partition plans, so the table is DEFAULT-only), a
// million current-month rows, older months, and an appender that keeps writing
// while the upgrade runs. The upgrade must finish inside the fenced step
// budgets, lose nothing, block no append for long, and leave only out-of-horizon
// rows in DEFAULT.
func TestMigrationUpgradesLiveInstanceEventTable(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var before []Migration
	for _, migration := range catalog {
		if migration.Version < 15 {
			before = append(before, migration)
		}
	}
	// #7853's runner applied the versioned migrations and nothing else: no
	// partition maintenance. (Runner.Apply now also reconciles grants against
	// the CURRENT inventory, which a pre-0015 schema cannot satisfy, so apply
	// the old catalog directly the way that runner's step did.)
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range before {
		if err := applyOne(t.Context(), conn, migration, "test"); err != nil {
			conn.Release()
			t.Fatalf("apply pre-0015 migration %d: %v", migration.Version, err)
		}
	}
	conn.Release()
	var partitions int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_inherits WHERE inhparent='journal.instance_event'::regclass`).Scan(&partitions); err != nil || partitions != 1 {
		t.Fatalf("legacy install must be DEFAULT-only, partitions=%d err=%v", partitions, err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO journal.instance_seq(instance_id,stream,last_seq,owner_epoch) VALUES
		  ('upgrade','scheduler',1,1), ('upgrade','concurrent',1,1);
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		SELECT 'upgrade','scheduler',g,clock_timestamp(),'x',
		       CASE WHEN g % 1000 = 0 THEN 'k'||g END,1,'\x7b7d'
		FROM generate_series(1,1000000) AS g;
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		SELECT 'upgrade','scheduler',2000000+g, clock_timestamp()-interval '40 days','x','old40-'||g,1,'\x7b7d' FROM generate_series(1,1000) g;
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		SELECT 'upgrade','scheduler',3000000+g, clock_timestamp()-interval '75 days','x',NULL,1,'\x7b7d' FROM generate_series(1,1000) g;
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		SELECT 'upgrade','scheduler',4000000+g, clock_timestamp()+interval '1 year','x',NULL,1,'\x7b7d' FROM generate_series(1,100) g`); err != nil {
		t.Fatal(err)
	}

	appenderConfig := pool.Config()
	appenderConfig.MaxConns = 2
	appendPool, err := pgxpool.NewWithConfig(t.Context(), appenderConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer appendPool.Close()
	var appended, appendErrors, raced atomic.Int64
	var slowest atomic.Int64
	stop := make(chan struct{})
	var appender sync.WaitGroup
	appender.Add(1)
	go func() {
		defer appender.Done()
		for seq := int64(1); ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			began := time.Now()
			_, err := appendPool.Exec(context.Background(), `
				INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
				VALUES ('upgrade','concurrent',$1,clock_timestamp(),'x',NULL,1,'\x7b7d')`, seq)
			if elapsed := time.Since(began).Nanoseconds(); elapsed > slowest.Load() {
				slowest.Store(elapsed)
			}
			if err != nil && strings.Contains(err.Error(), "violates partition constraint") {
				// The one-instant race with ATTACH of the current month. The
				// new binary retries it (journalpg.withInstance); so does this.
				raced.Add(1)
				_, err = appendPool.Exec(context.Background(), `
					INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
					VALUES ('upgrade','concurrent',$1,clock_timestamp(),'x',NULL,1,'\\x7b7d')`, seq)
			}
			if err != nil {
				if appendErrors.Add(1) == 1 {
					t.Logf("first append error: %v", err)
				}
				continue
			}
			appended.Add(1)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Phase 1: the migrate Job. The previous binary is still serving, so the
	// current month is deliberately left in DEFAULT (still fully visible).
	upgraded := Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "upgrade", Holder: "test"}
	began := time.Now()
	if err := upgraded.Apply(t.Context()); err != nil {
		t.Fatalf("migrate over live rows: %v", err)
	}
	t.Logf("migrate took %s", time.Since(began))
	var current int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM journal.instance_event
		WHERE tableoid='journal.instance_event_default'::regclass
		  AND at >= date_trunc('month', clock_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
		  AND at < (date_trunc('month', clock_timestamp() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current < 1000000 {
		t.Fatalf("migrate attached or moved the current month (DEFAULT holds %d current-month rows); that is the controller's job", current)
	}

	// Phase 2: the new controller's periodic pass moves the current month while
	// appends continue AND a reader checks, continuously, that every committed
	// event stays visible through the parent (a moved row that is not yet
	// attached would let a seq cursor skip it).
	manager, err := cloudlease.New(cloudlease.Config{Pool: pool, InstanceID: "upgrade", Name: "controller", Holder: "controller-a"})
	if err != nil {
		t.Fatal(err)
	}
	var hidden, polls atomic.Int64
	readStop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-readStop:
				return
			default:
			}
			var visible int64
			if err := appendPool.QueryRow(context.Background(), `
				SELECT count(*) FROM journal.instance_event
				WHERE instance_id='upgrade' AND stream='scheduler' AND seq <= 1000000`).Scan(&visible); err == nil {
				polls.Add(1)
				if visible != 1000000 {
					hidden.Add(1)
				}
			}
		}
	}()
	began = time.Now()
	var maintainer PartitionMaintainer
	// The controller holds (and renews) its lease for the whole pass.
	err = manager.Run(t.Context(), func(ctx context.Context, handle *cloudlease.Handle) error {
		maintainer = PartitionMaintainer{Pool: pool, Lease: manager,
			Handle: func() *cloudlease.Handle { return handle }, Plans: DefaultPartitionPlans}
		if err := maintainer.Ensure(ctx); err != nil {
			return err
		}
		return maintainer.Ensure(ctx) // idempotent reapply
	})
	close(readStop)
	reader.Wait()
	close(stop)
	appender.Wait()
	if err != nil {
		t.Fatalf("controller maintenance over %d live rows: %v", current, err)
	}
	t.Logf("controller pass took %s; reader polls %d, polls that saw hidden rows %d; concurrent appends %d, errors %d, attach races retried %d, slowest %s",
		time.Since(began), polls.Load(), hidden.Load(), appended.Load(), appendErrors.Load(), raced.Load(), time.Duration(slowest.Load()))
	if hidden.Load() != 0 {
		t.Fatalf("%d of %d reads missed committed events during the move", hidden.Load(), polls.Load())
	}
	if appendErrors.Load() != 0 {
		t.Fatalf("concurrent appender saw %d errors", appendErrors.Load())
	}
	var total, inDefault, keys int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*), count(*) FILTER (WHERE tableoid='journal.instance_event_default'::regclass)
		FROM journal.instance_event`).Scan(&total, &inDefault); err != nil {
		t.Fatal(err)
	}
	if want := 1000000 + 1000 + 1000 + 100 + appended.Load(); total != want {
		t.Fatalf("rows after upgrade = %d, want %d (nothing lost or duplicated)", total, want)
	}
	if inDefault != 100 {
		t.Fatalf("rows left in DEFAULT = %d, want only the 100 out-of-horizon rows", inDefault)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM journal.instance_emit_key`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 1000+1000 {
		t.Fatalf("backfilled emit keys = %d, want 2000", keys)
	}
}

// TestPartitionMaintenanceNeverHidesRowsOnFailedAttach: a writer holding an
// uncommitted insert on DEFAULT stops the table lock ATTACH needs. The pass
// fails, and every committed row must still be visible through the parent
// (and again after the writer finishes and a retry succeeds).
func TestPartitionMaintenanceNeverHidesRowsOnFailedAttach(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "hidden", Holder: "test",
		Partitions: []PartitionPlan{}}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO journal.instance_seq(instance_id,stream,last_seq,owner_epoch) VALUES ('hidden','scheduler',1,1);
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		SELECT 'hidden','scheduler',g,clock_timestamp(),'x',NULL,1,'\x7b7d' FROM generate_series(1,20000) g`); err != nil {
		t.Fatal(err)
	}
	visible := func() int64 {
		var n int64
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM journal.instance_event WHERE instance_id='hidden'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(t.Context(), `
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		VALUES ('hidden','scheduler',20001,clock_timestamp(),'x',NULL,1,'\x7b7d')`); err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{Pool: pool, InstanceID: "hidden", Name: "controller", Holder: "controller-a"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	maintainer := PartitionMaintainer{Pool: pool, Lease: manager,
		Handle: func() *cloudlease.Handle { return handle }, Plans: DefaultPartitionPlans}
	previous := partitionLockTries
	partitionLockTries = 2
	t.Cleanup(func() { partitionLockTries = previous })
	if err := maintainer.Ensure(t.Context()); err == nil {
		t.Fatal("maintenance attached while a writer held DEFAULT")
	}
	if got := visible(); got != 20000 {
		t.Fatalf("visible rows after a failed attach = %d, want 20000 (rows must never leave view)", got)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := maintainer.Ensure(t.Context()); err != nil {
		t.Fatalf("retry after the writer finished: %v", err)
	}
	if got := visible(); got != 20001 {
		t.Fatalf("visible rows after the retry = %d, want 20001", got)
	}
	var inDefault int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM journal.instance_event_default WHERE instance_id='hidden'`).Scan(&inDefault); err != nil || inDefault != 0 {
		t.Fatalf("rows left in DEFAULT after the retry = %d err=%v", inDefault, err)
	}
}

// TestPartitionMaintenanceRunsPeriodically drives the periodic job the control
// role runs: a dropped future month is re-created under the lease, and the
// retention drop removes the month's emit keys in the same transaction.
func TestPartitionMaintenanceRunsPeriodically(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "periodic", Holder: "test"}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	var suffix string
	var inMonth time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT to_char(date_trunc('month', clock_timestamp() AT TIME ZONE 'UTC') + interval '2 months','YYYYMM'),
		       (date_trunc('month', clock_timestamp() AT TIME ZONE 'UTC') + interval '2 months 1 day') AT TIME ZONE 'UTC'`).Scan(&suffix, &inMonth); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO journal.instance_seq(instance_id,stream,last_seq,owner_epoch) VALUES ('periodic','scheduler',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body)
		VALUES ('periodic','scheduler',1,$1,'x','retired-key',1,'\x7b7d')`, inMonth); err != nil {
		t.Fatal(err)
	}
	// The write path's trigger recorded the key.
	var keys int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM journal.instance_emit_key WHERE emit_key='retired-key'`).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("trigger-recorded keys = %d err=%v", keys, err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := DropInstanceEventPartition(t.Context(), tx, suffix); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM journal.instance_emit_key WHERE emit_key='retired-key'`).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("emit keys left after retention drop = %d err=%v", keys, err)
	}
	var exists bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass('journal.instance_event_'||$1) IS NOT NULL`, suffix).Scan(&exists); err != nil || exists {
		t.Fatalf("partition still exists after drop: %v %v", exists, err)
	}

	manager, err := cloudlease.New(cloudlease.Config{Pool: pool, InstanceID: "periodic", Name: "controller", Holder: "controller-a"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	maintainer := PartitionMaintainer{Pool: pool, Lease: manager,
		Handle: func() *cloudlease.Handle { return handle }, Plans: DefaultPartitionPlans}
	done := make(chan error, 1)
	var reported atomic.Int64
	go func() { done <- maintainer.Run(ctx, 50*time.Millisecond, func(error) { reported.Add(1) }) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := pool.QueryRow(ctx, `SELECT to_regclass('journal.instance_event_'||$1) IS NOT NULL`, suffix).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic maintenance did not re-create journal.instance_event_%s (reported errors: %d)", suffix, reported.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reported.Load() != 0 {
		t.Fatalf("periodic maintenance reported %d errors", reported.Load())
	}
}

// A superseded lease must stop maintenance before it moves or attaches anything.
func TestPartitionMaintenanceStopsOnLostLease(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "fenced", Holder: "test",
		Partitions: []PartitionPlan{}}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{Pool: pool, InstanceID: "fenced", Name: "controller", Holder: "controller-a"})
	if err != nil {
		t.Fatal(err)
	}
	stale := &cloudlease.Handle{InstanceID: "fenced", Name: "controller", Holder: "controller-a", Epoch: 999}
	maintainer := PartitionMaintainer{Pool: pool, Lease: manager,
		Handle: func() *cloudlease.Handle { return stale }, Plans: DefaultPartitionPlans}
	if err := maintainer.Ensure(t.Context()); err == nil {
		t.Fatal("maintenance succeeded without a valid lease")
	}
	var partitions int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_inherits WHERE inhparent='journal.instance_event'::regclass`).Scan(&partitions); err != nil || partitions != 1 {
		t.Fatalf("partitions after unfenced attempt = %d err=%v, want DEFAULT only", partitions, err)
	}
}
