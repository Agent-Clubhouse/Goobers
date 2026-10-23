//go:build cloudintegration

package objstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
)

func TestSurrenderWriteOnce(t *testing.T) {
	metadata, _, _ := testMetadata(t, "goobers-surrender")
	if err := metadata.PutSurrender(t.Context(), "instance", "run", "stage", 1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PutSurrender(t.Context(), "instance", "run", "stage", 1, []byte("first")); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if err := metadata.PutSurrender(t.Context(), "instance", "run", "stage", 1, []byte("different")); !errors.Is(err, ErrConflict) {
		t.Fatalf("different replay error = %v, want ErrConflict", err)
	}
}

func TestConfigApplyMonotonic(t *testing.T) {
	metadata, pool, _ := testMetadata(t, "goobers-config-apply")
	configDigest := digestFor([]byte("config"))
	if err := metadata.RecordGeneration(t.Context(), "instance", configDigest, []byte("config")); err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "instance", Name: "control", Holder: "controller",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apply := func(generation int64) bool {
		var applied bool
		err := manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
			var applyErr error
			applied, applyErr = metadata.ApplyConfig(ctx, tx, "instance", configDigest, generation)
			return applyErr
		})
		if err != nil {
			t.Fatal(err)
		}
		return applied
	}
	if !apply(2) {
		t.Fatal("new generation was not applied")
	}
	if apply(1) {
		t.Fatal("stale generation moved applied config backward")
	}
	var generation int64
	if err := pool.QueryRow(t.Context(), "SELECT generation FROM control.applied_config WHERE instance_id='instance'").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("generation = %d, want 2", generation)
	}
}

func TestPruneRefusesPinned(t *testing.T) {
	metadata, _, store := testMetadata(t, "goobers-config-prune")
	archive := []byte("config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PinGeneration(t.Context(), "instance", digest, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); !errors.Is(err, ErrPinned) {
		t.Fatalf("Prune error = %v, want ErrPinned", err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("pinned blob Has = %v, %v", has, err)
	}
}

func testMetadata(t *testing.T, container string) (*Metadata, *pgxpool.Pool, *Azure) {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := "object_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	identifier := pgx.Identifier{databaseName}.Sanitize()
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
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "object-test",
		InstanceID: "migration", Holder: "migration",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := testAzureStore(t, container)
	metadata, err := NewMetadata(pool, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return metadata, pool, store
}
