//go:build cloudintegration

package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
)

// stepTestPool creates a database private to the test so slow steps and lease
// takeovers cannot interfere with other packages sharing the server.
func stepTestPool(t *testing.T) *pgxpool.Pool {
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
	ensureRoles(t, pool)
	return pool
}

func catalogWith(t *testing.T, steps ...Migration) []Migration {
	t.Helper()
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		step.Kind, step.BinaryVersion, step.MinCompatibleBinary = Expand, "0.7.0", "0.6.0"
		catalog = append(catalog, step)
	}
	return catalog
}

func recorded(t *testing.T, pool *pgxpool.Pool, version int) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM control.schema_migration WHERE version=$1)", version,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// TestMigrationStepOutlivesFenceTimeout applies a step that runs longer than
// the 2s fenced-transaction bound.
func TestMigrationStepOutlivesFenceTimeout(t *testing.T) {
	pool := stepTestPool(t)
	runner := Runner{
		Direct: pool, BackupRef: "step-test", InstanceID: "instance", Holder: "migrate-a",
		Catalog: catalogWith(t, Migration{Version: 9001, Name: "9001_slow.expand.sql", SQL: `
			SELECT pg_sleep(2.5);
			COMMENT ON SCHEMA control IS 'slow migration step'`}),
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("slow migration step: %v", err)
	}
	if !recorded(t, pool, 9001) {
		t.Fatal("slow migration step was not recorded")
	}
}

// TestLostLeaseStopsMigrationSteps takes the migrate lease over while a step
// runs. The step rolls back and no later step is applied.
func TestLostLeaseStopsMigrationSteps(t *testing.T) {
	pool := stepTestPool(t)
	runner := Runner{
		Direct: pool, BackupRef: "step-test", InstanceID: "instance", Holder: "migrate-a",
		Catalog: catalogWith(t,
			Migration{Version: 9001, Name: "9001_slow.expand.sql", SQL: `
				SELECT pg_sleep(3);
				CREATE TABLE control.lost_first (id integer)`},
			Migration{Version: 9002, Name: "9002_next.expand.sql", SQL: `CREATE TABLE control.lost_second (id integer)`},
		),
	}
	done := make(chan error, 1)
	go func() { done <- runner.Apply(t.Context()) }()
	waitForSleepingStep(t, pool)

	successor, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "instance", Name: "migrate", Holder: "migrate-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	takeOver(t, pool, successor)

	var applyErr error
	select {
	case applyErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("migration did not stop after losing its lease")
	}
	if !errors.Is(applyErr, cloudlease.ErrStale) {
		t.Fatalf("apply after lease loss = %v, want ErrStale", applyErr)
	}
	for _, version := range []int{9001, 9002} {
		if recorded(t, pool, version) {
			t.Fatalf("migration %d was recorded after the lease was lost", version)
		}
	}
	var tables int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_tables
		WHERE schemaname='control' AND tablename IN ('lost_first', 'lost_second')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("%d step tables persisted after the lease was lost", tables)
	}
}

func waitForSleepingStep(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var sleeping bool
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid() AND datname=current_database()
				  AND query LIKE '%pg_sleep(3)%' AND state='active'
			)`).Scan(&sleeping); err != nil {
			t.Fatal(err)
		}
		if sleeping {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("migration step never started")
}

// takeOver expires the holder's heartbeat on the database clock and acquires
// the lease, retrying if the holder renewed in between.
func takeOver(t *testing.T, pool *pgxpool.Pool, successor *cloudlease.Manager) {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		if _, err := pool.Exec(t.Context(), `
			UPDATE control.lease_heartbeat SET expires_at=now()-interval '1 second'
			WHERE instance_id='instance' AND name='migrate'`); err != nil {
			t.Fatal(err)
		}
		_, err := successor.Acquire(t.Context())
		if err == nil {
			return
		}
		if !errors.Is(err, cloudlease.ErrHeld) {
			t.Fatal(err)
		}
	}
	t.Fatal("successor never acquired the migrate lease")
}
