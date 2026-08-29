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
// The superseded journal is never deleted before the replacement is in place:
// it is moved aside to a backup under the runs staging root, the staged
// directory is published and its parent synced, and only then is the backup
// removed. If publication fails the backup is renamed back to finalDir, so a
// rename, permission, or sharing failure can no longer destroy both the old
// journal and its replacement (#3641). When even the restore fails the error
// names the surviving backup path so an operator can recover it by hand.
//
// The run-dir lock is only probed and released again before the move aside (a
// held lock file cannot be renamed away on Windows); exclusion for the move
// itself comes from the publication lock, which every path that could take
// the run-dir lock acquires first.
//
// Returns whether stagedDir was actually published. On (true, nil) the caller
// still owns any further durability sync of finalDir's parent.
func ReplaceRun(finalDir, stagedDir string, keep func() (bool, error)) (bool, error) {
	publicationLock, err := acquireRunPublicationLock(finalDir)
	if err != nil {
		return false, err
	}
	defer releaseJournalLock(publicationLock)
	backupDir := ""
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
		backupDir, err = stashSupersededRun(finalDir)
		if err != nil {
			return false, err
		}
	}
	if err := os.Rename(stagedDir, finalDir); err != nil {
		publishErr := fmt.Errorf("journal: publish replacement run directory %s: %w", finalDir, err)
		if backupDir == "" {
			return false, publishErr
		}
		if restoreErr := os.Rename(backupDir, finalDir); restoreErr != nil {
			return false, fmt.Errorf(
				"%w; superseded run directory could not be restored and remains at %s: %w",
				publishErr, backupDir, restoreErr,
			)
		}
		_ = os.RemoveAll(filepath.Dir(backupDir))
		return false, publishErr
	}
	if backupDir != "" {
		// Make the published rename durable before the only other copy of the
		// journal goes away: a crash between the two must not be able to leave
		// the run directory missing entirely.
		if err := fsyncDir(filepath.Dir(finalDir)); err != nil {
			return false, fmt.Errorf(
				"journal: sync publication of run directory %s: %w; superseded run directory retained at %s",
				finalDir, err, backupDir,
			)
		}
		if err := os.RemoveAll(filepath.Dir(backupDir)); err != nil {
			return false, fmt.Errorf("journal: remove superseded run directory backup %s: %w", backupDir, err)
		}
	}
	return true, nil
}

// supersededBackupName is the leaf a superseded run directory is moved to
// inside its unique staging container. The container makes the destination
// path fresh, so the move aside is a rename onto a name that does not exist —
// the one form of directory rename that behaves the same on Windows as it
// does on POSIX.
const supersededBackupName = "superseded"

// stashSupersededRun moves the run directory at finalDir aside to a backup
// under the runs staging root — the same hidden sibling Create stages into,
// so the move stays on one filesystem and the backup is never mistaken for a
// published run — and returns the backup path.
func stashSupersededRun(finalDir string) (string, error) {
	stagingRoot := RunCreationStagingDir(filepath.Dir(finalDir))
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return "", fmt.Errorf("journal: create run replacement staging root %s: %w", stagingRoot, err)
	}
	container, err := os.MkdirTemp(stagingRoot, filepath.Base(finalDir)+"-superseded-")
	if err != nil {
		return "", fmt.Errorf("journal: create backup for superseded run directory %s: %w", finalDir, err)
	}
	backupDir := filepath.Join(container, supersededBackupName)
	if err := os.Rename(finalDir, backupDir); err != nil {
		_ = os.RemoveAll(container)
		return "", fmt.Errorf("journal: back up superseded run directory %s: %w", finalDir, err)
	}
	return backupDir, nil
}
