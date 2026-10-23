//go:build cloudintegration

package migrate

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresMigrationApply(t *testing.T) {
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), "DROP SCHEMA IF EXISTS control CASCADE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS control CASCADE")
	})
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Direct: pool, Catalog: catalog, BackupRef: "test-pitr-mark"}
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
}
