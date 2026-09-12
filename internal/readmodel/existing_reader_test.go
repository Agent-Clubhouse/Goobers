package readmodel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingReaderNeverCreatesOrMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	if reader, err := OpenExistingReader(ctx, path); err == nil {
		_ = reader.Close()
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read created database: %v", err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.writer.Exec("UPDATE projection_state SET schema_version = ?", len(migrations)-1); err != nil {
		t.Fatal(err)
	}
	if reader, err := OpenExistingReader(ctx, path); err == nil {
		_ = reader.Close()
		t.Fatal("old schema accepted")
	}
	state, err := store.State(ctx)
	if err != nil || state.SchemaVersion != len(migrations)-1 {
		t.Fatalf("read migrated database: %+v %v", state, err)
	}
}

func TestExistingReaderSeesLiveWALWithoutWriteSurface(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reader, err := OpenExistingReader(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if _, ok := any(reader).(Writer); ok {
		t.Fatal("read handle exposes projection writes")
	}
	if _, ok := any(reader).(FreshnessReporter); !ok {
		t.Fatal("read handle drops freshness metadata")
	}
	freshness, err := reader.ReadState(ctx, ReadStateInput{})
	if err != nil || freshness.Epoch == "" || len(freshness.Degraded) == 0 {
		t.Fatalf("missing freshness/unknown sweep evidence: %+v %v", freshness, err)
	}
	state, err := reader.State(ctx)
	if err != nil || state.Ready {
		t.Fatalf("initial state: %+v %v", state, err)
	}
	if err := store.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = reader.State(ctx)
	if err != nil || !state.Ready {
		t.Fatalf("committed WAL update hidden: %+v %v", state, err)
	}
	if _, _, err := reader.LatestQueueEligibility(ctx, "team", "review"); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.State(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed reader: %v", err)
	}
}
