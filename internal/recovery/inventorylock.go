package recovery

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// Why the inventory lock waits instead of failing on first contention (#5272).
//
// `.inventory.lock` is a single instance-wide lock that every publish, read,
// retire and reap takes for the duration of its own directory work. Two of
// those operations overlapping is not an error condition — it is the normal
// steady state of an instance running more than one run — yet every acquirer
// used bare TryAcquire, so the loser failed immediately with ErrHeld.
//
// The loser is often a run's TERMINAL cleanup (renewTerminalRecovery, reached
// through the worktree cleanup guard), and a failure there is not local: it
// propagates out as ErrCleanupDeferred, which leaves the run's active marker
// un-cleared. The observed cost on the cloud instance was 14 consecutive
// `stalled_run_sweep_failed` events and a `maxConcurrentRuns: 1` lane —
// backlog-curation, the only promoter of approved -> ready — going silently
// quiet for most of a day while every health signal stayed green.
//
// internal/platform/lock deliberately offers no blocking Acquire: a caller
// that waits forever can be wedged by a stale holder. The documented
// alternative is exactly this — retry TryAcquire against a bounded deadline —
// which internal/fleet's association lock already does. Contention resolves;
// a stale holder still fails, just as loudly as before and only later.

// inventoryLockWait bounds the wait. It has to cover a real holder's work, not
// just the handoff: RenewRetention holds the lock across an archive digest and
// RetireSnapshot across an owned-ref unpin. It stays short enough that an
// interactive `goobers` read cannot appear hung, and every caller's own context
// deadline still cuts it shorter.
//
// A var, not a const, only so the tests that pin "this operation holds the
// inventory lock" can assert ErrHeld without waiting the production budget out
// (setInventoryLockWaitForTest). Nothing in the tree reassigns it.
var inventoryLockWait = 15 * time.Second

// inventoryLockPoll is the retry interval, matching internal/fleet's.
const inventoryLockPoll = 10 * time.Millisecond

// acquireInventoryLock takes root's shared inventory lock, waiting out
// transient contention up to inventoryLockWait. It returns the underlying
// ErrHeld unchanged once the wait is spent, so callers and their tests keep
// seeing contention as contention.
func acquireInventoryLock(ctx context.Context, root string) (*platformlock.Handle, error) {
	return acquireLockWithin(ctx, filepath.Join(root, ".inventory.lock"), inventoryLockWait)
}

func acquireLockWithin(ctx context.Context, path string, wait time.Duration) (*platformlock.Handle, error) {
	deadline := time.Now().Add(wait)
	for {
		handle, err := platformlock.TryAcquire(path)
		if err == nil {
			return handle, nil
		}
		// Anything that is not contention — a missing directory, a permission
		// fault — is reported on the first attempt. Retrying it would only
		// delay the same answer.
		if !errors.Is(err, platformlock.ErrHeld) || !time.Now().Before(deadline) {
			return nil, err
		}
		timer := time.NewTimer(inventoryLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
