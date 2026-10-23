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

func TestLeaseContendersNeverOverlapEpochs(t *testing.T) {
	pool := testPool(t)
	paused := testManager(t, pool, "paused-holder")
	stale, err := paused.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
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
	wg.Wait()
	close(results)
	var winners []acquisition
	for result := range results {
		winners = append(winners, result)
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d, want 1", len(winners))
	}
	if _, err := pool.Exec(t.Context(), `
		CREATE TABLE control.fence_commit (
			epoch bigint PRIMARY KEY,
			holder text NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	winner := winners[0]
	if err := winner.manager.WithFence(t.Context(), winner.handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO control.fence_commit (epoch, holder) VALUES ($1, $2)", winner.handle.Epoch, winner.handle.Holder)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = paused.WithFence(t.Context(), stale, func(context.Context, pgx.Tx) error {
		t.Fatal("paused stale holder committed after takeover")
		return nil
	})
	if !errors.Is(err, lease.ErrStale) {
		t.Fatalf("paused holder error = %v, want ErrStale", err)
	}
	var commits int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM control.fence_commit").Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Fatalf("fenced commits = %d, want 1", commits)
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
