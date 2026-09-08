package recovery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
