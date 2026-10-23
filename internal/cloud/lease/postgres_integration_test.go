//go:build cloudintegration

package lease_test

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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
)

func TestLeaseRenewNotBlockedByFencedTx(t *testing.T) {
	pool := testPool(t)
	manager := testManager(t, pool, "holder-a")
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- manager.WithFence(t.Context(), handle, func(context.Context, pgx.Tx) error {
			close(entered)
			<-finish
			return nil
		})
	}()
	<-entered
	renewCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := manager.Renew(renewCtx, handle); err != nil {
		t.Fatalf("renew blocked by fenced transaction: %v", err)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
}

func TestFencedWriteRejectsStaleEpoch(t *testing.T) {
	pool := testPool(t)
	manager := testManager(t, pool, "holder-a")
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(t.Context()); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("duplicate acquisition error = %v, want ErrHeld", err)
	}
	if err := manager.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	err = manager.WithFence(t.Context(), handle, func(context.Context, pgx.Tx) error {
		t.Fatal("stale fenced write ran")
		return nil
	})
	if !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale fenced write error = %v, want ErrStale", err)
	}
}

func TestTakeoverWaitsForInflightFencedTx(t *testing.T) {
	pool := testPool(t)
	first := testManager(t, pool, "holder-a")
	handle, err := first.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	finish := make(chan struct{})
	fencedDone := make(chan error, 1)
	go func() {
		fencedDone <- first.WithFence(t.Context(), handle, func(context.Context, pgx.Tx) error {
			close(entered)
			<-finish
			return nil
		})
	}()
	<-entered
	expireHeartbeat(t, pool)
	second := testManager(t, pool, "holder-b")
	takeover := make(chan error, 1)
	go func() {
		_, err := second.Acquire(t.Context())
		takeover <- err
	}()
	select {
	case err := <-takeover:
		t.Fatalf("takeover completed before fenced transaction: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(finish)
	if err := <-fencedDone; err != nil {
		t.Fatal(err)
	}
	if err := <-takeover; err != nil {
		t.Fatal(err)
	}
}

func TestFencedTransactionCannotOutliveSelfFence(t *testing.T) {
	pool := testPool(t)
	if _, err := pool.Exec(t.Context(), "CREATE TABLE control.fence_duration (value integer)"); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "holder-a",
		TTL: 2 * time.Second, SelfFenceAfter: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
		for range 4 {
			if _, err := tx.Exec(ctx, "SELECT pg_sleep(0.12)"); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "INSERT INTO control.fence_duration VALUES (1)")
		return err
	})
	if !errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("long fenced transaction error = %v, want ErrSelfFenced", err)
	}
	var commits int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM control.fence_duration").Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Fatalf("long fenced transaction committed %d rows", commits)
	}
}

func TestFencedTransactionIsBoundedAcrossStatements(t *testing.T) {
	pool := testPool(t)
	if _, err := pool.Exec(t.Context(), "CREATE TABLE control.fence_statement_bound (value integer)"); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, pool, "holder-a")
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
		for range 4 {
			if _, err := tx.Exec(ctx, "SELECT pg_sleep(0.75)"); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "INSERT INTO control.fence_statement_bound VALUES (1)")
		return err
	})
	if !errors.Is(err, lease.ErrFencedTransactionTimeout) {
		t.Fatalf("multi-statement transaction error = %v, want ErrFencedTransactionTimeout", err)
	}
	var commits int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM control.fence_statement_bound").Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Fatalf("timed-out fenced transaction committed %d rows", commits)
	}
}

func TestBlockedRenewSelfFencesAtDeadline(t *testing.T) {
	pool := testPool(t)
	manager, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "holder-a",
		TTL: 2 * time.Second, RenewInterval: 100 * time.Millisecond,
		SelfFenceAfter: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan time.Time, 1)
	err = manager.Run(t.Context(), func(ctx context.Context, _ *lease.Handle) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM control.lease_heartbeat
			WHERE instance_id='instance' AND name='control'
			FOR UPDATE`); err != nil {
			return err
		}
		started <- time.Now()
		<-ctx.Done()
		return ctx.Err()
	})
	start := <-started
	elapsed := time.Since(start)
	if !errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("blocked renewal error = %v, want ErrSelfFenced", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 800*time.Millisecond {
		t.Fatalf("self-fence elapsed = %s, want approximately 500ms", elapsed)
	}
}

func TestReleaseAndAcquireUseSameLockOrder(t *testing.T) {
	pool := testPool(t)
	first := testManager(t, pool, "holder-a")
	handle, err := first.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	finish := make(chan struct{})
	fencedDone := make(chan error, 1)
	go func() {
		fencedDone <- first.WithFence(t.Context(), handle, func(context.Context, pgx.Tx) error {
			close(entered)
			<-finish
			return nil
		})
	}()
	<-entered
	expireHeartbeat(t, pool)
	second := testManager(t, pool, "holder-b")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	released := make(chan error, 1)
	acquired := make(chan error, 1)
	go func() { released <- first.Release(ctx, handle) }()
	go func() {
		_, err := second.Acquire(ctx)
		acquired <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(finish)
	if err := <-fencedDone; err != nil {
		t.Fatal(err)
	}
	releaseErr, acquireErr := <-released, <-acquired
	if isDeadlock(releaseErr) || isDeadlock(acquireErr) {
		t.Fatalf("release/acquire deadlocked: release=%v acquire=%v", releaseErr, acquireErr)
	}
	if acquireErr != nil {
		t.Fatalf("acquire after concurrent release: %v", acquireErr)
	}
	if releaseErr != nil && !errors.Is(releaseErr, lease.ErrStale) {
		t.Fatalf("concurrent release: %v", releaseErr)
	}
}

func TestTakeoverExpiryStartsAfterLockWait(t *testing.T) {
	pool := testPool(t)
	first := testManager(t, pool, "holder-a")
	handle, err := first.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	fencedDone := make(chan error, 1)
	go func() {
		fencedDone <- first.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
			close(entered)
			_, err := tx.Exec(ctx, "SELECT pg_sleep(1.5)")
			return err
		})
	}()
	<-entered
	expireHeartbeat(t, pool)
	second, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "holder-b",
		TTL: time.Second, SelfFenceAfter: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		_, err := second.Acquire(t.Context())
		acquired <- err
	}()
	if err := <-fencedDone; err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	var remaining float64
	if err := pool.QueryRow(t.Context(), `
		SELECT extract(epoch FROM (expires_at-clock_timestamp()))
		FROM control.lease_heartbeat
		WHERE instance_id='instance' AND name='control'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining < 0.8 {
		t.Fatalf("takeover lease remaining = %.3fs, want a fresh 1s TTL", remaining)
	}
}

func TestLeaseContendersNeverOverlapEpochs(t *testing.T) {
	pool := testPool(t)
	if _, err := pool.Exec(t.Context(), `
		CREATE TABLE control.fence_commit (
			epoch bigint PRIMARY KEY,
			holder text NOT NULL,
			started_at timestamptz NOT NULL,
			finished_at timestamptz NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	observer, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Release()

	paused, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "paused-holder",
		TTL: 2 * time.Second, SelfFenceAfter: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := paused.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	resume := make(chan struct{})
	staleCommit := make(chan error, 1)
	pausedDone := make(chan error, 1)
	go func() {
		pausedDone <- paused.WithFence(t.Context(), stale, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				INSERT INTO control.fence_commit (epoch, holder, started_at, finished_at)
				VALUES ($1, $2, clock_timestamp(), 'infinity')`,
				stale.Epoch, stale.Holder,
			); err != nil {
				return err
			}
			close(entered)
			<-resume
			err := tx.Commit(context.Background())
			staleCommit <- err
			return err
		})
	}()
	<-entered
	expireHeartbeat(t, pool)

	const contenders = 8
	var wg sync.WaitGroup
	type acquisition struct {
		handle  *lease.Handle
		manager *lease.Manager
	}
	results := make(chan acquisition, contenders)
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			offset := time.Duration(i-contenders/2) * time.Hour
			manager, err := lease.New(lease.Config{
				Pool: pool, InstanceID: "instance", Name: "control",
				Holder: fmt.Sprintf("contender-%d", i),
				Now:    func() time.Time { return time.Now().Add(offset) },
			})
			if err != nil {
				t.Error(err)
				return
			}
			handle, err := manager.Acquire(t.Context())
			if err == nil {
				results <- acquisition{handle: handle, manager: manager}
				return
			}
			if !errors.Is(err, lease.ErrHeld) {
				t.Errorf("contender %d: %v", i, err)
			}
		}(i)
	}
	waitForTakeoverLock(t, observer)
	var winner acquisition
	select {
	case winner = <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("no contender took over after the paused holder self-fenced")
	}
	if !paused.SelfFenced(stale) {
		t.Fatal("contender took over before the paused holder self-fenced")
	}
	close(resume)
	commitErr := <-staleCommit
	if commitErr == nil {
		t.Fatal("stale holder commit succeeded after takeover")
	}
	var commitPostgresError *pgconn.PgError
	if errors.As(commitErr, &commitPostgresError) && commitPostgresError.Code != "25P03" {
		t.Fatalf("stale commit SQLSTATE = %s, want 25P03", commitPostgresError.Code)
	}
	if err := <-pausedDone; !errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("paused holder error = %v, want ErrSelfFenced", err)
	}
	wg.Wait()
	close(results)
	winners := []acquisition{winner}
	for result := range results {
		winners = append(winners, result)
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d, want 1", len(winners))
	}
	winner = winners[0]
	if err := winner.manager.WithFence(t.Context(), winner.handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO control.fence_commit (epoch, holder, started_at, finished_at)
			VALUES ($1, $2, clock_timestamp(), clock_timestamp())`,
			winner.handle.Epoch, winner.handle.Holder,
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var staleRow bool
	if err := pool.QueryRow(t.Context(), `
		SELECT EXISTS (SELECT 1 FROM control.fence_commit WHERE epoch=$1)`,
		stale.Epoch,
	).Scan(&staleRow); err != nil {
		t.Fatal(err)
	}
	if staleRow {
		t.Fatal("stale holder row committed after takeover")
	}
	staleVerifier, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: stale.Holder,
		Now: func() time.Time { return time.Now().Add(-time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = staleVerifier.WithFence(t.Context(), stale, func(context.Context, pgx.Tx) error {
		t.Fatal("stale handle write callback ran")
		return nil
	})
	if !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale handle write error = %v, want ErrStale", err)
	}
	rows, err := pool.Query(t.Context(), `
		SELECT epoch, started_at, finished_at
		FROM control.fence_commit ORDER BY epoch`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type commit struct {
		epoch           int64
		started, finish time.Time
	}
	var commits []commit
	for rows.Next() {
		var current commit
		if err := rows.Scan(&current.epoch, &current.started, &current.finish); err != nil {
			t.Fatal(err)
		}
		commits = append(commits, current)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("fenced commits = %d, want only the successor epoch", len(commits))
	}
	if commits[0].epoch != winner.handle.Epoch || commits[0].epoch <= stale.Epoch {
		t.Fatalf("stale or overlapping epoch committed: %+v", commits)
	}
}

func TestRunCancellationVoluntarilyReleases(t *testing.T) {
	pool := testPool(t)
	first := testManager(t, pool, "holder-a")
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- first.Run(ctx, func(runCtx context.Context, _ *lease.Handle) error {
			close(entered)
			<-runCtx.Done()
			return runCtx.Err()
		})
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lease run error = %v", err)
	}
	second := testManager(t, pool, "holder-b")
	if _, err := second.Acquire(t.Context()); err != nil {
		t.Fatalf("voluntary release did not permit immediate handover: %v", err)
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
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := "lease_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	databaseIdentifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+databaseIdentifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+databaseIdentifier); err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = lease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "DROP SCHEMA IF EXISTS control CASCADE"); err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "lease-test",
		InstanceID: "migration", Holder: "migration-test",
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS control CASCADE")
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+databaseIdentifier+" WITH (FORCE)")
		admin.Close()
	})
	return pool
}

func testManager(t *testing.T, pool *pgxpool.Pool, holder string) *lease.Manager {
	t.Helper()
	manager, err := lease.New(lease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: holder,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func expireHeartbeat(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.lease_heartbeat
		SET expires_at=now()-interval '1 second'
		WHERE instance_id='instance' AND name='control'`); err != nil {
		t.Fatal(err)
	}
}

func isDeadlock(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "40P01"
}

func waitForTakeoverLock(t *testing.T, observer *pgxpool.Conn) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		var waiting bool
		if err := observer.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND wait_event_type='Lock'
				  AND query LIKE '%UPDATE control.lease%'
			)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no contender reached the authority-row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
