//go:build cloudintegration

package journal_test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func postgresJournalConfig(t *testing.T) journalpg.Config {
	return postgresJournalConfigRenew(t, true)
}

func postgresJournalConfigRenew(t *testing.T, renew bool) journalpg.Config {
	t.Helper()
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set (local PG16 required)")
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatal(err)
	}
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
func postgresBackend(t *testing.T, config journalpg.Config) *journalpg.Backend {
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
		ctx := context.Background()
		for _, runID := range []string{"run-alpha", "run-zeta"} {
			empty, emptyOwner, err := writer.CreateRun(ctx, journal.CreateRunRequest{
				Identity: journal.RunIdentity{RunID: runID, Gaggle: "g", Workflow: "implementation"},
			})
			if err != nil {
				return nil, err
			}
			if err := writer.Retire(ctx, empty, emptyOwner); err != nil {
				return nil, err
			}
		}
		handle, owner, err := writer.CreateRun(ctx, journal.CreateRunRequest{
			Identity: journal.RunIdentity{RunID: "run-reader", Gaggle: "g", Workflow: "implementation"},
		})
		if err != nil {
			return nil, err
		}
		if _, err := writer.AppendBatch(ctx, handle, owner, []journal.Event{
			{Type: journal.EventRunnerAnnotation, Name: "first"},
		}); err != nil {
			return nil, err
		}
		ref, err := writer.RecordBlob(ctx, handle, owner, journal.BlobRecord{
			Event: journal.Event{Type: journal.EventArtifactRecorded, Name: "blob", Integrity: apiv1.IntegrityDerived},
			Data:  []byte("reader-blob"),
		})
		if err != nil {
			return nil, err
		}
		_ = ref
		if err := writer.Retire(ctx, handle, owner); err != nil {
			return nil, err
		}
		instance, instanceOwner, _, err := writer.OpenInstance(ctx)
		if err != nil {
			return nil, err
		}
		if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventDaemonStarted}); err != nil {
			return nil, err
		}
		if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventRunnerAnnotation, Name: "second"}); err != nil {
			return nil, err
		}
		if err := writer.RetireInstance(ctx, instance, instanceOwner); err != nil {
			return nil, err
		}

		return backend, nil
	}); err != nil {
		t.Fatal(err)
	}
}
