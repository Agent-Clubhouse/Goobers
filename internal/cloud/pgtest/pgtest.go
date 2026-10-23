//go:build cloudintegration

// Package pgtest provisions migrated PostgreSQL databases and real leases for
// cloud integration tests. It is compiled only with the cloudintegration tag.
package pgtest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
)

// InstanceID is the instance every helper lease belongs to.
const InstanceID = "instance"

// Pool creates a fresh database named after the test, applies the embedded
// catalog, and drops the database on cleanup. It skips without
// GOOBERS_TEST_POSTGRES_URL.
func Pool(t *testing.T, prefix string) *pgxpool.Pool {
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
	name := strings.ToLower(prefix + "_" + strings.ReplaceAll(t.Name(), "/", "_"))
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
	config.ConnConfig.Database = name
	config.MaxConns = lease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	EnsureRoles(t, pool)
	runner := migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: prefix + "-test",
		InstanceID: "migration", Holder: "migration-test",
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}

// Manager returns a lease manager for holder whose host clock is offset by
// skew; the lease itself is measured on the database clock.
func Manager(t *testing.T, pool *pgxpool.Pool, holder string, skew time.Duration) *lease.Manager {
	t.Helper()
	manager, err := lease.New(lease.Config{
		Pool: pool, InstanceID: InstanceID, Name: "control", Holder: holder,
		Now: func() time.Time { return time.Now().Add(skew) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// Takeover expires the live heartbeat on the database clock and acquires the
// lease for successor, returning the successor's handle.
func Takeover(t *testing.T, pool *pgxpool.Pool, successor *lease.Manager) *lease.Handle {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.lease_heartbeat SET expires_at=now()-interval '1 second'
		WHERE instance_id=$1 AND name='control'`, InstanceID); err != nil {
		t.Fatal(err)
	}
	handle, err := successor.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

// Acquire takes the lease for manager.
func Acquire(t *testing.T, manager *lease.Manager) *lease.Handle {
	t.Helper()
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return handle
}
