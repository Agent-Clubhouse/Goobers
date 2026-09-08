package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const retentionFileName = "retention.json"

// ReadRetainedRecord overlays a separately published retention extension on
// immutable capture metadata. No identity or archive binding may change.
func ReadRetainedRecord(path string) (Record, error) {
	original, err := ReadRecord(path)
	if err != nil {
		return Record{}, err
	}
	extended, err := ReadRecord(filepath.Join(filepath.Dir(path), retentionFileName))
	if errors.Is(err, os.ErrNotExist) {
		return original, nil
	}
	if err != nil {
		return Record{}, err
	}
	comparison := extended
	comparison.RetainUntil = original.RetainUntil
	if comparison != original || extended.RetainUntil.Before(original.RetainUntil) {
		return Record{}, ErrRecordConflict
	}
	return extended, nil
}

// RenewRetention extends only the lifetime of verified archive bytes. The
// caller must derive deadline from a durable terminal event, never retry time.
// A single fixed-size sidecar bounds storage across retries and later resumes.
// Success is not journal acknowledgement; the coordinator must append it.
func RenewRetention(ctx context.Context, path string, deadline time.Time, maxBytes int64) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if deadline.IsZero() {
		return Record{}, fmt.Errorf("empty recovery renewal deadline")
	}
	directory := filepath.Dir(path)
	// Match publication's inventory-before-snapshot lock order. A retention
	// sweep must be able to move/remove a reservation without a concurrent
	// renewal writing through the old directory name or replacing its lock.
	inventoryLock, err := platformlock.TryAcquire(filepath.Join(filepath.Dir(directory), ".inventory.lock"))
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = inventoryLock.Release() }()
	handle, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = handle.Release() }()
	record, err := ReadRetainedRecord(path)
	if err != nil {
		return Record{}, err
	}
	digest, err := archiveDigest(filepath.Join(directory, BundleFileName), maxBytes)
	if err != nil {
		return Record{}, err
	}
	info, err := os.Lstat(filepath.Join(directory, BundleFileName))
	if err != nil || info.Size() != record.ArchiveBytes || digest != record.ArchiveDigest {
		return Record{}, fmt.Errorf("recovery renewal archive does not match its binding")
	}
	if deadline.After(record.RetainUntil) {
		record.RetainUntil = deadline.UTC()
	}
	data, err := Encode(record)
	if err != nil {
		return Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	// Reflush identical retries too: a prior rename may have succeeded before
	// its directory flush failed. Never shorten an already acknowledged window.
	if err := journal.WriteFileAtomic(filepath.Join(directory, retentionFileName), data, 0o600); err != nil {
		return Record{}, err
	}
	return record, nil
}
