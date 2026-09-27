package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// A holder that releases while the wait is still running must not fail the
// waiter. This is the #5272 defect in miniature: the loser of a routine
// overlap on the instance-wide inventory lock was failing outright, and when
// the loser was a run's terminal cleanup that failure left the run's active
// marker pinned and its maxConcurrentRuns: 1 lane wedged.
func TestAcquireInventoryLockWaitsOutTransientContention(t *testing.T) {
	setInventoryLockWaitForTest(t, 5*time.Second)
	root := t.TempDir()
	holder, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		if releaseErr := holder.Release(); releaseErr != nil {
			t.Errorf("release holder: %v", releaseErr)
		}
		close(released)
	}()

	handle, err := acquireInventoryLock(context.Background(), root)
	if err != nil {
		t.Fatalf("contention that cleared within the budget still failed: %v", err)
	}
	<-released
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
}

// The wait is bounded, not indefinite: a holder that never lets go still
// reports ErrHeld, unchanged, so a stale holder cannot wedge a caller forever
// (the reason internal/platform/lock offers no blocking Acquire at all).
func TestAcquireInventoryLockStillReportsHeldAfterTheBudget(t *testing.T) {
	setInventoryLockWaitForTest(t, 20*time.Millisecond)
	root := t.TempDir()
	holder, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	start := time.Now()
	if handle, err := acquireInventoryLock(context.Background(), root); !errors.Is(err, platformlock.ErrHeld) {
		if handle != nil {
			_ = handle.Release()
		}
		t.Fatalf("permanent holder ignored: %v", err)
	}
	if waited := time.Since(start); waited < 20*time.Millisecond {
		t.Fatalf("gave up before the budget was spent: waited %s", waited)
	}
}

// Cancellation wins over the wait, so a caller's own deadline still bounds it.
func TestAcquireInventoryLockHonorsCancellationWhileWaiting(t *testing.T) {
	setInventoryLockWaitForTest(t, time.Minute)
	root := t.TempDir()
	holder, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if handle, err := acquireInventoryLock(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		if handle != nil {
			_ = handle.Release()
		}
		t.Fatalf("waiting acquirer ignored its context: %v", err)
	}
}

// A fault that is not contention is reported on the first attempt rather than
// retried for the full budget: the answer cannot change.
func TestAcquireInventoryLockDoesNotRetryNonContentionFaults(t *testing.T) {
	setInventoryLockWaitForTest(t, time.Minute)
	root := filepath.Join(t.TempDir(), "absent")
	start := time.Now()
	if handle, err := acquireInventoryLock(context.Background(), root); err == nil {
		_ = handle.Release()
		t.Fatal("missing inventory root accepted")
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("non-contention fault was retried: waited %s", waited)
	}
}

// The end-to-end shape: a renewal that loses a race for the inventory lock now
// completes once the winner is done, instead of reporting the failure that
// propagates out of terminal cleanup as ErrCleanupDeferred.
func TestRenewRetentionSurvivesTransientInventoryContention(t *testing.T) {
	setInventoryLockWaitForTest(t, 5*time.Second)
	root := t.TempDir()
	record := storageTestRecord()
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(make([]byte, record.ArchiveBytes)))
	directory := seedInventoryRecord(t, root, record)
	path := filepath.Join(directory, RecordFileName)

	holder, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		if releaseErr := holder.Release(); releaseErr != nil {
			t.Errorf("release holder: %v", releaseErr)
		}
		close(released)
	}()

	deadline := record.RetainUntil.Add(time.Hour)
	got, err := RenewRetention(context.Background(), path, deadline, 1024)
	if err != nil || !got.RetainUntil.Equal(deadline) {
		t.Fatalf("renewal lost a race it should have waited out: %+v %v", got, err)
	}
	<-released
}
