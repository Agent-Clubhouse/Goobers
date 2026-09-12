package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestRetireSnapshotPreservesArchiveAndInventoryBound(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	directory := seedInventoryRecord(t, root, record)
	called := false
	retired, err := RetireSnapshot(context.Background(), root, record, func(got Record) error {
		called = true
		if got != record {
			t.Fatal("wrong pin identity")
		}
		if _, err := ReadRecord(filepath.Join(directory, RecordFileName)); err != nil {
			t.Fatalf("archive moved before unpin: %v", err)
		}
		if _, err := ReadInventory(context.Background(), root, 1); !errors.Is(err, platformlock.ErrHeld) {
			t.Fatalf("retirement did not hold inventory lock: %v", err)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("retirement: %q %v called=%t", retired, err, called)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired snapshot remains active: %v", err)
	}
	if got, err := ReadRecord(filepath.Join(retired, RecordFileName)); err != nil || got != record {
		t.Fatalf("retirement lost metadata: %+v %v", got, err)
	}
	if entries, err := ReadInventory(context.Background(), root, 1); err != nil || len(entries) != 0 {
		t.Fatalf("retired snapshot exposed as restorable: %+v %v", entries, err)
	}
	if _, err := reserveSnapshotDirectory(root, "another", 1); !errors.Is(err, ErrInventoryFull) {
		t.Fatalf("retirement escaped inventory bound: %v", err)
	}
	// Simulate a crash after metadata removal during subsequent file cleanup.
	if err := os.Remove(filepath.Join(retired, RecordFileName)); err != nil {
		t.Fatal(err)
	}
	if entries, err := ReadInventory(context.Background(), root, 1); err != nil || len(entries) != 0 {
		t.Fatalf("interrupted retirement broke reads: %+v %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(retired, "unknown"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventory(context.Background(), root, 1); err == nil {
		t.Fatal("unknown retired contents were hidden")
	}
}

func TestRetireSnapshotRefusesChangedRecordAndFailedUnpin(t *testing.T) {
	for _, mode := range []string{"renewed", "unpin-failed", "locked"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			record := storageTestRecord()
			directory := seedInventoryRecord(t, root, record)
			if mode == "renewed" {
				extended := record
				extended.RetainUntil = extended.RetainUntil.Add(time.Hour)
				if err := PublishRecord(filepath.Join(directory, retentionFileName), extended); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "locked" {
				lock, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lock.Release() }()
			}
			called := false
			retired, err := RetireSnapshot(context.Background(), root, record, func(Record) error {
				called = true
				return errors.New("pin cleanup failed")
			})
			if err == nil || retired != "" || called != (mode == "unpin-failed") {
				t.Fatalf("unsafe retirement: %q %v called=%t", retired, err, called)
			}
			if _, err := os.Stat(filepath.Join(directory, BundleFileName)); err != nil {
				t.Fatalf("refusal lost archive: %v", err)
			}
		})
	}
}
