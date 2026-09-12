package journal

import (
	"errors"
	"fmt"
	"path/filepath"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// ErrRecoveryBusy means another writer or publisher owns this journal.
// Cleanup callers must retain their unimported evidence and retry later.
var ErrRecoveryBusy = errors.New("journal: recovery is busy")

// TryRecover opens an existing current-schema journal for recovery without
// waiting for a publisher or writer. Unlike Recover it does not migrate an
// old schema: cleanup must preserve evidence when migration is required.
func TryRecover(dir string, opts ...Option) (*Run, RecoverReport, error) {
	options := append([]Option(nil), opts...)
	options = append(options, func(c *config) { c.tryRecoveryLocks = true })
	return recover(dir, false, options...)
}

func tryRecoveryLock(path, location, target string) (*journalLock, error) {
	held, err := platformlock.TryAcquire(path)
	if errors.Is(err, platformlock.ErrHeld) {
		return nil, fmt.Errorf("%w: %s at %s", ErrRecoveryBusy, target, location)
	}
	return held, err
}

func openRecoveryReader(dir string, tryOnly bool) (*Reader, error) {
	if tryOnly {
		return OpenReadOnly(dir)
	}
	return OpenRead(dir)
}

func acquireRecoveryPublicationLock(dir string, tryOnly bool) (*journalLock, error) {
	if tryOnly {
		return acquireRunPublicationLockWith(dir, tryRecoveryLock)
	}
	return acquireRunPublicationLock(dir)
}

func acquireRecoveryRunLock(dir string, tryOnly bool) (*journalLock, error) {
	if tryOnly {
		return tryRecoveryLock(filepath.Join(dir, fileLock), dir, "run")
	}
	return acquireRunLock(dir)
}
