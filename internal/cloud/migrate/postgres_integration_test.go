//go:build cloudintegration

package migrate

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
