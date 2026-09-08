package recovery

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// ErrRecordConflict means a publication would replace previously retained
// recovery identity. Callers must preserve the original snapshot and record.
var ErrRecordConflict = errors.New("recovery record already contains different state")

// PublishRecord durably publishes immutable metadata at a caller-owned path.
// The parent directory must already exist and be private to the instance.
// It creates no per-run directory or retention policy: the capture coordinator
// must bound its inventory and durably retain the objects BEFORE publishing.
// Success here alone never authorizes removal of a worktree or recovery ref.
// Concurrent publication returns the platform lock's busy error for retry.
func PublishRecord(path string, record Record) error {
	data, err := Encode(record)
	if err != nil {
		return err
	}
	handle, err := platformlock.TryAcquire(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock recovery record: %w", err)
	}
	defer func() { _ = handle.Release() }()
	existing, err := ReadRecord(path)
	if err == nil {
		encoded, err := Encode(existing)
		if err != nil {
			return err
		}
		if !bytes.Equal(encoded, data) {
			return ErrRecordConflict
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Re-publish identical bytes too: a previous attempt may have renamed the
	// record but failed its directory flush. Mere readability is not durability.
	if err := journal.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("publish recovery record: %w", err)
	}
	return nil
}

// ReadRecord reads bounded metadata without following a final-component link.
// A corrupted or replaced record never yields a partial recovery identity.
func ReadRecord(path string) (Record, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return Record{}, fmt.Errorf("inspect recovery record: %w", err)
	}
	if !before.Mode().IsRegular() {
		return Record{}, fmt.Errorf("recovery record is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Record{}, fmt.Errorf("open recovery record: %w", err)
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil {
		return Record{}, fmt.Errorf("inspect opened recovery record: %w", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return Record{}, fmt.Errorf("recovery record changed while opening")
	}
	return Decode(file)
}
