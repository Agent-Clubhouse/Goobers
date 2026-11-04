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

func seedInventoryRecord(t *testing.T, root string, record Record) string {
	t.Helper()
	directory, err := reserveSnapshotDirectory(root, inventoryDirectoryName(record), MaxInventoryEntries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, BundleFileName), make([]byte, record.ArchiveBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishRecord(filepath.Join(directory, RecordFileName), record); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestReadInventoryBoundedAndIdentityChecked(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	directory := seedInventoryRecord(t, root, record)
	entries, err := ReadInventory(context.Background(), root, 1)
	if err != nil || len(entries) != 1 || entries[0].Record != record || entries[0].RecordPath != filepath.Join(directory, RecordFileName) {
		t.Fatalf("inventory = %+v, error %v", entries, err)
	}
	if err := os.Mkdir(filepath.Join(root, "partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	if entries, err := ReadInventory(context.Background(), root, 1); !errors.Is(err, ErrInventoryFull) || entries != nil {
		t.Fatalf("overflow returned partial success: %+v %v", entries, err)
	}
	if entries, err := ReadInventory(context.Background(), root, 2); err == nil || entries != nil {
		t.Fatalf("incomplete reservation returned partial success: %+v %v", entries, err)
	}
}

// A crashed publish leaves a reservation directory holding only lock files.
// Strict ReadInventory refuses the whole scan for it — correct, since no
// caller deciding what is safe to discard may see a partial inventory — but
// the eviction hook then failed FOREVER, so capacity was never reclaimed and
// the inventory grew without bound (#5092). The tolerant scan must return the
// readable entries anyway, and name the broken one rather than hide it.
func TestReadInventoryTolerantSkipsBrokenReservation(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	seedInventoryRecord(t, root, record)
	broken := filepath.Join(root, "b0000000000000000000000000000000000000000000000000000000000000ff")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".publish.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventory(context.Background(), root, 10); err == nil {
		t.Fatal("strict read accepted a broken reservation; eviction's tolerance would be untested")
	}
	entries, unreadable, err := ReadInventoryTolerant(context.Background(), root, 10)
	if err != nil {
		t.Fatalf("tolerant read failed on a broken reservation: %v", err)
	}
	if len(entries) != 1 || entries[0].Record != record {
		t.Fatalf("tolerant read lost the readable entry: %+v", entries)
	}
	if len(unreadable) != 1 || unreadable[0].Name != filepath.Base(broken) || unreadable[0].Err == nil {
		t.Fatalf("tolerant read did not report the broken reservation: %+v", unreadable)
	}
}

func TestReadInventoryRefusesMisfiledRecord(t *testing.T) {
	root := t.TempDir()
	directory := seedInventoryRecord(t, root, storageTestRecord())
	if err := os.Rename(directory, filepath.Join(root, "foreign")); err != nil {
		t.Fatal(err)
	}
	if entries, err := ReadInventory(context.Background(), root, 1); !errors.Is(err, ErrRecordConflict) || entries != nil {
		t.Fatalf("misfiled record accepted: %+v %v", entries, err)
	}
}

func TestReadInventoryHonorsCancellationAndPublisherLock(t *testing.T) {
	setInventoryLockWaitForTest(t, time.Millisecond)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadInventory(ctx, root, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	handle, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()
	if _, err := ReadInventory(context.Background(), root, 1); !errors.Is(err, platformlock.ErrHeld) {
		t.Fatalf("publisher lock ignored: %v", err)
	}
}

func TestReadInventoryMissingRootDoesNotCreateIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if entries, err := ReadInventory(context.Background(), root, 1); err != nil || len(entries) != 0 {
		t.Fatalf("missing inventory: %+v %v", entries, err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created inventory: %v", err)
	}
}

func TestReadInventoryRefusesMissingOrWrongSizeArchive(t *testing.T) {
	for _, missing := range []bool{false, true} {
		root := t.TempDir()
		directory := seedInventoryRecord(t, root, storageTestRecord())
		archive := filepath.Join(directory, BundleFileName)
		if missing {
			if err := os.Remove(archive); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(archive, []byte("incomplete"), 0o600); err != nil {
			t.Fatal(err)
		}
		if entries, err := ReadInventory(context.Background(), root, 1); err == nil || entries != nil {
			t.Fatalf("missing=%v: unavailable archive accepted: %+v %v", missing, entries, err)
		}
		if _, err := ReadRecord(filepath.Join(directory, RecordFileName)); err != nil {
			t.Fatalf("failed inspection discarded metadata: %v", err)
		}
	}
}

func TestReadInventoryReconcilesStaleRetentionTemp(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	directory := seedInventoryRecord(t, root, record)
	extended := record
	extended.RetainUntil = extended.RetainUntil.Add(time.Hour)
	data, err := Encode(extended)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, retentionFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(directory, "."+retentionFileName+".tmp-123")
	if err := os.WriteFile(temp, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadInventory(context.Background(), root, 1)
	if err != nil || len(entries) != 1 || entries[0].Record != record {
		t.Fatalf("inventory with stale retention temp = %+v, %v", entries, err)
	}
	if _, err := os.Lstat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale retention temp survived reconciliation: %v", err)
	}
	if got, err := ReadRetainedRecord(entries[0].RecordPath); err != nil || got != extended {
		t.Fatalf("retention floor changed during reconciliation: %+v, %v", got, err)
	}
}

func TestReadInventoryDoesNotRaceActiveRetentionWriter(t *testing.T) {
	root := t.TempDir()
	directory := seedInventoryRecord(t, root, storageTestRecord())
	temp := filepath.Join(directory, "."+retentionFileName+".tmp-456")
	if err := os.WriteFile(temp, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()

	if entries, err := ReadInventory(context.Background(), root, 1); !errors.Is(err, platformlock.ErrHeld) || entries != nil {
		t.Fatalf("active retention writer was bypassed: %+v, %v", entries, err)
	}
	if data, err := os.ReadFile(temp); err != nil || string(data) != "in flight" {
		t.Fatalf("active writer temp changed: %q, %v", data, err)
	}
}

func TestReadInventoryRejectsUnknownOrNonRegularRetentionTemp(t *testing.T) {
	for _, name := range []string{"operator-evidence", "." + retentionFileName + ".tmp-789"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			directory := seedInventoryRecord(t, root, storageTestRecord())
			path := filepath.Join(directory, name)
			if name == "operator-evidence" {
				if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}

			if entries, err := ReadInventory(context.Background(), root, 1); err == nil || entries != nil {
				t.Fatalf("unrecognized reservation content accepted: %+v, %v", entries, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("unrecognized reservation content removed: %v", err)
			}
		})
	}
}

func TestReadInventoryRejectsSymlinkRetentionTemp(t *testing.T) {
	root := t.TempDir()
	directory := seedInventoryRecord(t, root, storageTestRecord())
	target := filepath.Join(root, "operator-evidence")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(directory, "."+retentionFileName+".tmp-987")
	if err := os.Symlink(target, temp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if entries, err := ReadInventory(context.Background(), root, 1); err == nil || entries != nil {
		t.Fatalf("symlink retention temp accepted: %+v, %v", entries, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "preserve" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}
