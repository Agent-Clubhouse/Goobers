//go:build cloudintegration

package objstore

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

func TestPruneDefersBlobUntilRetentionSweep(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-retention")
	archive := []byte("retained config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("prune synchronously removed blob: Has=%v err=%v", has, err)
	}
	manager, handle := gcLease(t, pool)
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("GC deleted %d blobs inside retention window", deleted)
	}
}

func TestGCSweepRepublishRaceKeepsPinnedBlob(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-config-gc-race")
	archive := []byte("shared config archive")
	digest := digestFor(archive)
	if err := metadata.RecordGeneration(t.Context(), "old-instance", digest, archive); err != nil {
		t.Fatal(err)
	}
	if err := metadata.PruneGeneration(t.Context(), "old-instance", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.blob_gc_candidate
		SET unreferenced_at=clock_timestamp()-interval '2 hours'
		WHERE digest=$1`,
		digest,
	); err != nil {
		t.Fatal(err)
	}
	pausing := &pausingStore{
		Azure: store, removing: make(chan struct{}), resume: make(chan struct{}),
	}
	metadata.blobs = pausing
	manager, handle := gcLease(t, pool)
	sweepDone := make(chan error, 1)
	go func() {
		_, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
		sweepDone <- err
	}()
	<-pausing.removing

	republishDone := make(chan error, 1)
	go func() {
		if err := metadata.RecordGeneration(t.Context(), "new-instance", digest, archive); err != nil {
			republishDone <- err
			return
		}
		republishDone <- metadata.PinGeneration(t.Context(), "new-instance", digest, "active-run")
	}()
	select {
	case err := <-republishDone:
		t.Fatalf("republish escaped blob lock before delete completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(pausing.resume)
	if err := <-sweepDone; err != nil {
		t.Fatal(err)
	}
	if err := <-republishDone; err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("republished pinned blob Has=%v err=%v", has, err)
	}
	var pins int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.config_generation_pin
		WHERE instance_id='new-instance' AND digest=$1`,
		digest,
	).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 1 {
		t.Fatalf("new generation pins = %d, want 1", pins)
	}
}

func gcLease(t *testing.T, pool *pgxpool.Pool) (*cloudlease.Manager, *cloudlease.Handle) {
	t.Helper()
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "instance", Name: "gc", Holder: "gc-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return manager, handle
}

type pausingStore struct {
	*Azure
	removing chan struct{}
	resume   chan struct{}
}

func (s *pausingStore) Remove(ctx context.Context, digest string) error {
	close(s.removing)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.resume:
	}
	return s.Azure.Remove(ctx, digest)
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
