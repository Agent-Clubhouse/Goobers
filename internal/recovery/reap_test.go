package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReapRetiredRestoresBoundedCapacityAfterInterruptedCleanup(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		record := storageTestRecord()
		directory := seedInventoryRecord(t, root, record)
		retired, err := RetireSnapshot(context.Background(), root, record, func(Record) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := os.Remove(filepath.Join(retired, BundleFileName)); err != nil {
				t.Fatal(err)
			}
		}
		results, err := ReapRetired(context.Background(), root, 1, false)
		if err != nil || len(results) != 1 || !results[0].DryRun || results[0].Deleted {
			t.Fatalf("dry-run: %+v %v", results, err)
		}
		if _, err := os.Stat(retired); err != nil {
			t.Fatalf("dry-run removed candidate: %v", err)
		}
		results, err = ReapRetired(context.Background(), root, 1, true)
		if err != nil || len(results) != 1 || !results[0].Deleted {
			t.Fatalf("reap: %+v %v", results, err)
		}
		if _, err := os.Stat(retired); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reap left candidate: %v", err)
		}
		if results, err := ReapRetired(context.Background(), root, 1, true); err != nil || len(results) != 0 {
			t.Fatalf("retry: %+v %v", results, err)
		}
		if _, err := reserveSnapshotDirectory(root, filepath.Base(directory), 1); err != nil {
			t.Fatalf("capacity not reclaimed: %v", err)
		}
	}
}

func TestReapRetiredPreservesActiveAndUnknownContents(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	directory := seedInventoryRecord(t, root, record)
	if results, err := ReapRetired(context.Background(), root, 1, true); err != nil || len(results) != 0 {
		t.Fatalf("active snapshot was considered: %+v %v", results, err)
	}
	retired, err := RetireSnapshot(context.Background(), root, record, func(Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "operator-evidence"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := ReapRetired(context.Background(), root, 1, true)
	if err == nil || len(results) != 1 || results[0].Err == nil || results[0].Deleted {
		t.Fatalf("unknown contents silently removed: %+v %v", results, err)
	}
	if _, err := os.Stat(filepath.Join(retired, BundleFileName)); err != nil {
		t.Fatalf("failed preflight removed archive: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaper recreated active directory: %v", err)
	}
}
