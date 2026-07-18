package journal

import (
	"errors"
	"testing"
	"time"
)

// TestAcquireJournalLockTimesOutWhenHeld is the regression guard for the
// abort↔daemon deadlock: a second acquire of a lock another holder owns must
// fail with ErrLockTimeout within the bound, not block in the flock syscall
// forever (the bare blocking LOCK_EX that wedged `goobers run abort` against a
// live daemon holding a run's journal lock for its lifetime).
func TestAcquireJournalLockTimesOutWhenHeld(t *testing.T) {
	dir := t.TempDir()

	held, err := acquireJournalLock(dir, "run")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer releaseJournalLock(held)

	prevTimeout, prevPoll := journalLockTimeout, journalLockPollInterval
	journalLockTimeout, journalLockPollInterval = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { journalLockTimeout, journalLockPollInterval = prevTimeout, prevPoll })

	start := time.Now()
	f, err := acquireJournalLock(dir, "run")
	elapsed := time.Since(start)
	if err == nil {
		releaseJournalLock(f)
		t.Fatal("second acquire succeeded while the lock was held; want ErrLockTimeout")
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	// Gave up near the bound — neither hung nor returned instantly.
	if elapsed < journalLockTimeout {
		t.Fatalf("returned after %s, before the %s bound — did it actually wait?", elapsed, journalLockTimeout)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s — bound not enforced (would have hung in production)", elapsed)
	}
}

// TestAcquireJournalLockSucceedsWhenFree confirms the common path is unchanged:
// an uncontended acquire returns immediately, and a later acquire succeeds once
// the first releases.
func TestAcquireJournalLockSucceedsWhenFree(t *testing.T) {
	dir := t.TempDir()

	f1, err := acquireJournalLock(dir, "run")
	if err != nil {
		t.Fatalf("acquire on free lock: %v", err)
	}
	releaseJournalLock(f1)

	f2, err := acquireJournalLock(dir, "run")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	releaseJournalLock(f2)
}

// TestAcquireJournalLockWaitsThenWinsAfterRelease confirms a contended waiter
// still gets the lock (waits its turn) when the holder releases before the
// bound — the serialize-don't-fail behavior run.go's acquireRunLock doc and
// TestRecoverSerializesConcurrentWriters rely on, preserved by the bounded loop.
func TestAcquireJournalLockWaitsThenWinsAfterRelease(t *testing.T) {
	dir := t.TempDir()

	held, err := acquireJournalLock(dir, "run")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	prevTimeout, prevPoll := journalLockTimeout, journalLockPollInterval
	journalLockTimeout, journalLockPollInterval = 5*time.Second, 10*time.Millisecond
	t.Cleanup(func() { journalLockTimeout, journalLockPollInterval = prevTimeout, prevPoll })

	time.AfterFunc(150*time.Millisecond, func() { releaseJournalLock(held) })

	f, err := acquireJournalLock(dir, "run")
	if err != nil {
		t.Fatalf("waiter did not win the lock after release: %v", err)
	}
	releaseJournalLock(f)
}
