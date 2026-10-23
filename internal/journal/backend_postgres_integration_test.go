//go:build cloudintegration

package journal_test

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
	"github.com/goobers/goobers/internal/journal"
)

func postgresJournalConfig(t testing.TB) journalpg.Config {
	t.Helper()
	return postgresJournalConfigRenew(t, true)
}

func postgresJournalConfigRenew(t testing.TB, renew bool) journalpg.Config {
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

// postgresOwnerPool opens an owner session on the fixture database for test
// setup the controller role is not granted (DDL, raw storage edits).
func postgresOwnerPool(t testing.TB, config journalpg.Config) *pgxpool.Pool {
	t.Helper()
	cfg := config.Pool.Config()
	cfg.AfterConnect = nil
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func postgresBackend(t testing.TB, config journalpg.Config) *journalpg.Backend {
	t.Helper()
	backend, err := journalpg.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}
func TestJournalBackendConformancePostgres(t *testing.T) {
	t.Run("pg", func(t *testing.T) {
		if err := journalBackendConformance(func() backendStore {
			config := postgresJournalConfig(t)
			return backendStore{Open: func() journal.Backend { return newCleanupBackend(t, postgresBackend(t, config)) }}
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestJournalReaderBackendConformancePostgres(t *testing.T) {
	if err := journalReaderBackendConformance(func() (journal.ReaderBackend, error) {
		config := postgresJournalConfig(t)
		backend := postgresBackend(t, config)
		writer := newCleanupBackend(t, backend)
		if err := seedReaderBackend(writer); err != nil {
			return nil, err
		}
		// The storage adapter injects a future envelope field after retirement.
		// A writer that only knows today's Event cannot produce that field.
		if _, err := postgresOwnerPool(t, config).Exec(context.Background(), `UPDATE journal.run_event SET body=$1 WHERE instance_id=$2 AND run_id='run-reader' AND seq=2`, []byte(unknownReaderRecord), config.InstanceID); err != nil {
			return nil, err
		}
		return backend, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkPostgresJournalAppend is a report-only nightly measurement. It has no
// throughput threshold; correctness remains covered by the race/conformance gate.
func BenchmarkPostgresJournalAppend(b *testing.B) {
	config := postgresJournalConfig(b)
	backend := postgresBackend(b, config)
	handle, owner, err := backend.CreateRun(b.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: "run-benchmark", Gaggle: "g", Workflow: "benchmark"}})
	if err != nil {
		b.Fatal(err)
	}
	events := []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "throughput"}}
	for b.Loop() {
		if _, err := backend.AppendBatch(b.Context(), handle, owner, events); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "events/s")
}
