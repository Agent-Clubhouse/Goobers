//go:build cloudintegration

package journalequivalence

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgroles"
	"github.com/goobers/goobers/internal/cloud/pgtest"
)

func cloudJournalConfig(t testing.TB) journalpg.Config {
	t.Helper()
	return cloudJournalConfigRenew(t, true)
}

func cloudJournalConfigRenew(t testing.TB, renew bool) journalpg.Config {
	t.Helper()
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set (local PG16 required)")
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, admin)
	database := "journal_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg := admin.Config()
	cfg.ConnConfig.Database = database
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	migration := migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "local-test", InstanceID: "migration", Holder: "test"}
	if err := migration.Apply(t.Context()); err != nil {
		t.Fatalf("apply journal fixture migrations: %v", err)
	}
	// The backend and its lease run as the controller role production uses, so
	// a write path missing from the grant inventory fails here.
	pool = rolePool(t, pool, pgroles.Controller)
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "test-instance", Name: "controller", Holder: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !renew {
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := manager.Renew(ctx, handle); err != nil && ctx.Err() == nil {
					t.Errorf("renew test lease: %v", err)
					return
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done; _ = manager.Release(context.Background(), handle) })
	return journalpg.Config{Pool: pool, InstanceID: "test-instance", Lease: manager, LiveOwner: func() *lease.Handle { return handle }}
}

// rolePool opens a pool on base's database whose sessions act as role.
func rolePool(t testing.TB, base *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
