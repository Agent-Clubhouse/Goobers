package intake

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingReaderDoesNotCreateMissingIntake(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	reader, err := OpenExistingReader(context.Background(), path)
	if err == nil {
		_ = reader.Close()
		t.Fatal("missing intake database accepted")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("read-only open created intake database: %v", statErr)
	}
}

func TestExistingReaderReportsPendingWithoutWriteCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Observed(context.Background(), "run-1", 7); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenExistingReader(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if count, err := reader.Count(context.Background()); err != nil || count != 1 {
		t.Fatalf("Count = %d, %v; want 1", count, err)
	}
	if _, ok := any(reader).(interface {
		Observed(context.Context, string, uint64) error
	}); ok {
		t.Fatal("read-only intake handle exposes writes")
	}
}

func TestExistingReaderFenceChangesAfterExternalCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reader, err := OpenExistingReader(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	before, err := reader.Fence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Observed(context.Background(), "run-1", 1); err != nil {
		t.Fatal(err)
	}
	after, err := reader.Fence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.Pending != 0 || after.Pending != 1 || before.DataVersion == after.DataVersion {
		t.Fatalf("fences before=%+v after=%+v, want pending/data-version change", before, after)
	}
}
