package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryReservationsAreBoundedAndRetryable(t *testing.T) {
	root := t.TempDir()
	first, err := reserveSnapshotDirectory(root, "first", 1)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := reserveSnapshotDirectory(root, "first", 1); err != nil || retry != first {
		t.Fatalf("retry consumed another reservation: %q %v", retry, err)
	}
	if _, err := reserveSnapshotDirectory(root, "second", 1); !errors.Is(err, ErrInventoryFull) {
		t.Fatalf("inventory limit ignored: %v", err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatal("full inventory evicted original recovery reservation")
	}
}

func TestInventoryUnknownEntriesConsumeCapacity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "partial-evidence"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveSnapshotDirectory(root, "new", 1); !errors.Is(err, ErrInventoryFull) {
		t.Fatalf("partial evidence bypassed inventory bound: %v", err)
	}
}

func TestInventoryRefusesNonDirectoryReservation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveSnapshotDirectory(root, "existing", 1); err == nil {
		t.Fatal("file accepted as snapshot directory")
	}
}

func TestInventoryRetryPreservesAndRefusesCrashDebris(t *testing.T) {
	root := t.TempDir()
	directory, err := reserveSnapshotDirectory(root, "snapshot", 1)
	if err != nil {
		t.Fatal(err)
	}
	debris := filepath.Join(directory, "unfinished-archive")
	if err := os.WriteFile(debris, []byte("recovery evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveSnapshotDirectory(root, "snapshot", 1); err == nil {
		t.Fatal("retry could accumulate additional crash debris")
	}
	if data, err := os.ReadFile(debris); err != nil || string(data) != "recovery evidence" {
		t.Fatalf("crash evidence discarded: %q %v", data, err)
	}
}
