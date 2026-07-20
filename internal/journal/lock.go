package journal

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// journalLockTimeout bounds how long acquireJournalLock waits for a contended
// exclusive lock before giving up with ErrLockTimeout, instead of blocking in
// the platform lock forever. A bare blocking acquire meant a second opener of a
// run/instance dir a live daemon already holds — the exact case `goobers run
// abort` hits, since it deliberately skips up.lock (see cmd/goobers/run.go) and
// the daemon holds a run's journal lock for that run's whole lifetime (see
// run.go's acquireRunLock doc) — hung indefinitely (observed: `goobers run
// abort` wedged in the lock syscall while the daemon held the run dir's
// .lock).
//
// The bound is generous enough that legitimate transient contention always wins
// its turn well within it: the instance log's per-append acquire/release is
// sub-millisecond, and a single daemon never self-contends on a run lock (the
// up.lock keeps it single-instance; Create uses a fresh run id and in-process
// resume closes its writer before reopening). A lock held for a run's lifetime
// by a separate process instead surfaces as an actionable error rather than a
// silent hang. A var, not a const, so tests can shrink it without a real wait;
// production never mutates it.
var journalLockTimeout = 30 * time.Second

// journalLockPollInterval is how often a contended, non-blocking lock is
// retried while waiting its turn — short enough that a waiter proceeds promptly
// once the holder releases, long enough not to busy-spin.
var journalLockPollInterval = 50 * time.Millisecond

// ErrLockTimeout reports that acquireJournalLock could not take the lock within
// journalLockTimeout because another process holds it — typically a running
// goobers daemon that owns this run/instance for its lifetime. Callers (e.g.
// `goobers run abort`) can surface an actionable "stop the daemon first"
// message instead of appearing to hang.
var ErrLockTimeout = errors.New("journal: lock held by another process (a running daemon?)")

type journalLock = platformlock.Handle

func acquireJournalLock(dir, target string) (*journalLock, error) {
	path := filepath.Join(dir, fileLock)
	// A non-blocking acquire retried on a short poll up to journalLockTimeout,
	// rather than a bare blocking acquire, so a lock a live daemon holds for a
	// run's lifetime can never wedge a second opener forever. Mirrors the
	// bounded, retry-based lock cmd/goobers's instance lock already uses.
	deadline := time.Now().Add(journalLockTimeout)
	for {
		held, lockErr := platformlock.TryAcquire(path)
		if lockErr == nil {
			return held, nil
		}
		if !errors.Is(lockErr, platformlock.ErrHeld) {
			return nil, fmt.Errorf("journal: acquire %s lock: %w", target, lockErr)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("journal: acquire %s lock at %s within %s: %w", target, dir, journalLockTimeout, ErrLockTimeout)
		}
		time.Sleep(journalLockPollInterval)
	}
}

func releaseJournalLock(held *journalLock) {
	if held == nil {
		return
	}
	_ = held.Release()
}
