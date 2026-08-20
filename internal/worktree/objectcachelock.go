package worktree

import (
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

// objectCacheLockTimeout bounds how long acquireObjectCacheLock waits for a
// contended object-cache lock before giving up with ErrObjectCacheLockTimeout,
// instead of blocking forever. This used to call lock.Acquire, whose blocking
// mode has no timeout at the OS level, so a stale holder (a crashed or wedged
// process from another gaggle's Manager — the cache is shared node-wide, see
// ensureObjectCache's doc) held the cache clone/fetch/GC path forever, with no
// diagnostic and nothing to time out. Mirrors the bounded, retry-based lock
// internal/journal's own acquireJournalLockPath already uses for the same
// reason (#2889); lock.Acquire itself is gone now that every caller in the
// repository has migrated to a bounded TryAcquire retry loop.
var objectCacheLockTimeout = 30 * time.Second

// objectCacheLockPollInterval is how often a contended, non-blocking lock is
// retried while waiting its turn.
var objectCacheLockPollInterval = 50 * time.Millisecond

// ErrObjectCacheLockTimeout reports that acquireObjectCacheLock could not take
// the lock within objectCacheLockTimeout because another process holds it.
var ErrObjectCacheLockTimeout = errors.New("worktree: object cache lock held by another process")

// acquireObjectCacheLock takes the object cache's file lock, retrying a
// non-blocking acquire on a short poll up to objectCacheLockTimeout rather
// than blocking indefinitely.
func acquireObjectCacheLock(path string) (*lock.Handle, error) {
	deadline := time.Now().Add(objectCacheLockTimeout)
	for {
		held, err := lock.TryAcquire(path)
		if err == nil {
			return held, nil
		}
		if !errors.Is(err, lock.ErrHeld) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("acquire object cache lock at %s within %s: %w", path, objectCacheLockTimeout, ErrObjectCacheLockTimeout)
		}
		time.Sleep(objectCacheLockPollInterval)
	}
}

// setObjectCacheLockTimeoutForTest overrides objectCacheLockTimeout /
// objectCacheLockPollInterval for the duration of a test, returning a restore
// func. Production code never calls this.
func setObjectCacheLockTimeoutForTest(timeout, poll time.Duration) (restore func()) {
	prevTimeout, prevPoll := objectCacheLockTimeout, objectCacheLockPollInterval
	objectCacheLockTimeout, objectCacheLockPollInterval = timeout, poll
	return func() { objectCacheLockTimeout, objectCacheLockPollInterval = prevTimeout, prevPoll }
}
