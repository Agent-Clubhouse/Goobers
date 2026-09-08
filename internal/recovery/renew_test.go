package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestRenewRetentionRejectsOverflowingArchiveBudget(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	// A max-int byte budget must not overflow the reader's extra-byte probe
	// and turn verification of a nonempty archive into a hash of zero bytes.
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(nil))
	directory := seedInventoryRecord(t, root, record)
	got, err := RenewRetention(context.Background(), filepath.Join(directory, RecordFileName), record.RetainUntil.Add(time.Hour), math.MaxInt64)
	if err == nil || got != (Record{}) {
		t.Fatalf("overflowing budget bypassed archive verification: %+v %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, retentionFileName)); !os.IsNotExist(err) {
		t.Fatalf("invalid verification published a deadline: %v", err)
	}
}

func TestRenewRetentionHonorsCancellationAndPublicationLock(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(make([]byte, record.ArchiveBytes)))
	directory := seedInventoryRecord(t, root, record)
	path := filepath.Join(directory, RecordFileName)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := RenewRetention(ctx, path, record.RetainUntil.Add(time.Hour), 1024); !errors.Is(err, context.Canceled) || got != (Record{}) {
		t.Fatalf("cancelled renewal acknowledged: %+v %v", got, err)
	}
	handle, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()
	if got, err := RenewRetention(context.Background(), path, record.RetainUntil.Add(time.Hour), 1024); !errors.Is(err, platformlock.ErrHeld) || got != (Record{}) {
		t.Fatalf("renewal bypassed publisher: %+v %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, retentionFileName)); !os.IsNotExist(err) {
		t.Fatalf("refused renewal published a deadline: %v", err)
	}
}

func TestRenewRetentionHonorsInventoryLock(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(make([]byte, record.ArchiveBytes)))
	directory := seedInventoryRecord(t, root, record)
	path := filepath.Join(directory, RecordFileName)
	handle, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()
	deadline := record.RetainUntil.Add(time.Hour)
	if got, err := RenewRetention(context.Background(), path, deadline, 1024); !errors.Is(err, platformlock.ErrHeld) || got != (Record{}) {
		t.Fatalf("renewal bypassed inventory transaction: %+v %v", got, err)
	}
	if got, err := ReadRetainedRecord(path); err != nil || got != record {
		t.Fatalf("refused renewal changed retention: %+v %v", got, err)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
	if got, err := RenewRetention(context.Background(), path, deadline, 1024); err != nil || !got.RetainUntil.Equal(deadline) {
		t.Fatalf("renewal failed after inventory transaction completed: %+v %v", got, err)
	}
}

func TestRenewRetentionPreservesCaptureAndNeverShortens(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(make([]byte, record.ArchiveBytes)))
	directory := seedInventoryRecord(t, root, record)
	path := filepath.Join(directory, RecordFileName)
	deadline := record.RetainUntil.Add(45 * 24 * time.Hour)
	for _, want := range []time.Time{deadline, deadline, record.RetainUntil} {
		got, err := RenewRetention(context.Background(), path, want, 1024)
		if err != nil || !got.RetainUntil.Equal(deadline) {
			t.Fatalf("renewal = %+v %v", got, err)
		}
		if original, err := ReadRecord(path); err != nil || original != record {
			t.Fatalf("renewal changed immutable capture: %+v %v", original, err)
		}
		if effective, err := ReadRetainedRecord(path); err != nil || effective != got {
			t.Fatalf("restore cannot read renewed window: %+v %v", effective, err)
		}
	}
	if _, err := ReadInventory(context.Background(), root, 1); err != nil {
		t.Fatalf("renewal broke bounded inventory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, BundleFileName), make([]byte, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := RenewRetention(context.Background(), path, deadline.Add(time.Hour), 1024); err == nil || got != (Record{}) {
		t.Fatalf("corrupt archive received renewal: %+v %v", got, err)
	}
	if effective, err := ReadRetainedRecord(path); err != nil || !effective.RetainUntil.Equal(deadline) {
		t.Fatalf("failed renewal changed acknowledged deadline: %+v %v", effective, err)
	}
}

func TestRetentionSidecarCannotChangeArchiveIdentity(t *testing.T) {
	directory := t.TempDir()
	record := storageTestRecord()
	path := filepath.Join(directory, RecordFileName)
	if err := PublishRecord(path, record); err != nil {
		t.Fatal(err)
	}
	foreign := record
	foreign.ArchiveBytes++
	if err := PublishRecord(filepath.Join(directory, retentionFileName), foreign); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRetainedRecord(path); err == nil || got != (Record{}) {
		t.Fatalf("foreign renewal accepted: %+v %v", got, err)
	}
}
