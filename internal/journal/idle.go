package journal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// WithIdleRunReader invokes fn under the run's publication and writer locks.
// Contention returns false without waiting. The reader is not migrated or
// repaired; missing, obsolete, or pruning-reserved journals return errors.
// Callers must check identity and terminality under these locks before deleting
// associated resources. An idle writer does not imply a terminal run.
// The callback must not open a writer or reacquire either journal lock, and the
// reader must not be used after the callback returns.
func WithIdleRunReader(ctx context.Context, dir string, fn func(*Reader) error) (bool, error) {
	if fn == nil {
		return false, fmt.Errorf("journal: idle run reader requires callback")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := OpenReadOnly(dir); err != nil {
		return false, err
	}
	publication, err := acquireRunPublicationLockWith(dir, func(path, _, _ string) (*journalLock, error) {
		return platformlock.TryAcquire(path)
	})
	if errors.Is(err, platformlock.ErrHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer releaseJournalLock(publication)
	writer, err := platformlock.TryAcquireExisting(filepath.Join(dir, fileLock))
	if errors.Is(err, platformlock.ErrHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer releaseJournalLock(writer)
	reader, err := openCurrentJournal(dir)
	if err != nil {
		return false, err
	}
	reserved, err := PruneReserved(dir)
	if err != nil {
		return false, err
	}
	if reserved {
		return false, ErrPruneReserved
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, fn(reader)
}
