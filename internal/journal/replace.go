package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// ErrRunActive reports that a run directory could not be replaced because a
// live writer currently holds its run-dir lock. The caller retries after the
// writer closes (for the reconciler: the next cycle) instead of deleting the
// directory out from under an open journal.
var ErrRunActive = errors.New("journal: run directory is held open by a live writer")

// ReplaceRun publishes stagedDir as the run directory at finalDir, replacing
// whatever partial journal is there — the repair path a re-projection uses —
// with the same coordination every writer-opening path already observes:
//
//   - the run publication lock is held across the whole
//     recheck → remove → rename span, so no Create or Recover of the same run
//     can begin opening finalDir mid-replacement (both acquire that lock
//     first);
//   - a live writer already holding finalDir's run-dir lock refuses the
//     replacement with ErrRunActive. Removing the directory under an open
//     writer detaches its lock file and events log into unlinked inodes: the
//     writer keeps acknowledging appends that no reader of the published
//     directory can ever see (#3529);
//   - keep, when non-nil, is consulted under those locks with any writer
//     excluded. Returning true aborts the replacement and leaves finalDir as
//     found — the journal was finished by another owner while the caller
//     staged its replacement — reported as (false, nil).
//
// The run-dir lock is only probed and released again before the removal (a
// held lock file cannot be deleted on Windows); exclusion for the removal
// itself comes from the publication lock, which every path that could take
// the run-dir lock acquires first.
//
// Returns whether stagedDir was actually published. On (true, nil) the caller
// still owns the durability sync of finalDir's parent.
func ReplaceRun(finalDir, stagedDir string, keep func() (bool, error)) (bool, error) {
	publicationLock, err := acquireRunPublicationLock(finalDir)
	if err != nil {
		return false, err
	}
	defer releaseJournalLock(publicationLock)
	if Recorded(finalDir) {
		probe, lockErr := platformlock.TryAcquire(filepath.Join(finalDir, fileLock))
		if lockErr != nil {
			if errors.Is(lockErr, platformlock.ErrHeld) {
				return false, fmt.Errorf("journal: replace run directory %s: %w", finalDir, ErrRunActive)
			}
			return false, fmt.Errorf("journal: probe run lock for %s: %w", finalDir, lockErr)
		}
		releaseJournalLock(probe)
		if keep != nil {
			keepExisting, keepErr := keep()
			if keepErr != nil {
				return false, keepErr
			}
			if keepExisting {
				return false, nil
			}
		}
		if err := os.RemoveAll(finalDir); err != nil {
			return false, fmt.Errorf("journal: remove superseded run directory %s: %w", finalDir, err)
		}
	}
	if err := os.Rename(stagedDir, finalDir); err != nil {
		return false, fmt.Errorf("journal: publish replacement run directory %s: %w", finalDir, err)
	}
	return true, nil
}
