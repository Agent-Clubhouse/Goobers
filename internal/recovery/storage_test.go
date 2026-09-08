package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func storageTestRecord() Record {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	return Record{Version: 1, RunID: "run-1", RepositoryKey: "github|||team|repo|", Ref: "refs/goobers/recovery/run-1", BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), CreatedAt: now, RetainUntil: now.Add(time.Hour)}
}

func TestPublishRecordIsImmutableAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	want := storageTestRecord()
	if err := PublishRecord(path, want); err != nil {
		t.Fatal(err)
	}
	if err := PublishRecord(path, want); err != nil {
		t.Fatalf("identical retry failed: %v", err)
	}
	changed := want
	changed.SnapshotSHA = strings.Repeat("d", 40)
	if err := PublishRecord(path, changed); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("conflicting retry accepted: %v", err)
	}
	got, err := ReadRecord(path)
	if err != nil || got != want {
		t.Fatalf("original recovery identity lost: %+v %v", got, err)
	}
}

func TestPublishRecordPreservesCorruptMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishRecord(path, storageTestRecord()); err == nil {
		t.Fatal("corrupt metadata silently replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "corrupt" {
		t.Fatalf("original evidence lost: %q %v", got, err)
	}
}

func TestPublishRecordHonorsExistingPublisherLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	handle, err := platformlock.TryAcquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()
	if err := PublishRecord(path, storageTestRecord()); !errors.Is(err, platformlock.ErrHeld) {
		t.Fatalf("publisher lock ignored: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record written without lock: %v", err)
	}
}

func TestReadRecordRejectsNonRegularAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadRecord(root); err == nil {
		t.Fatal("directory accepted as record")
	}
	path := filepath.Join(root, "large.json")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", MaxRecordBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRecord(path); err == nil || got != (Record{}) {
		t.Fatalf("oversized record accepted: %+v %v", got, err)
	}
}
