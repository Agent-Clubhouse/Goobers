package worktree

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

// TestAcquireObjectCacheLockTimesOutWhenHeld is the regression guard for
// #2905: a second acquire of a lock another holder owns must fail with
// ErrObjectCacheLockTimeout within the bound, not block in the flock syscall
// forever the way lock.Acquire's bare blocking mode did.
func TestAcquireObjectCacheLockTimesOutWhenHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.lock")

	held, err := acquireObjectCacheLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	restore := setObjectCacheLockTimeoutForTest(200*time.Millisecond, 10*time.Millisecond)
	defer restore()

	start := time.Now()
	f, err := acquireObjectCacheLock(path)
	elapsed := time.Since(start)
	if err == nil {
		_ = f.Release()
		t.Fatal("second acquire succeeded while the lock was held; want ErrObjectCacheLockTimeout")
	}
	if !errors.Is(err, ErrObjectCacheLockTimeout) {
		t.Fatalf("err = %v, want ErrObjectCacheLockTimeout", err)
	}
	if elapsed < objectCacheLockTimeout {
		t.Fatalf("returned after %s, before the %s bound — did it actually wait?", elapsed, objectCacheLockTimeout)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s — bound not enforced (would have hung in production)", elapsed)
	}
}

// TestAcquireObjectCacheLockSucceedsWhenFree confirms the common path is
// unchanged: an uncontended acquire returns immediately, and a later acquire
// succeeds once the first releases.
func TestAcquireObjectCacheLockSucceedsWhenFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.lock")

	f1, err := acquireObjectCacheLock(path)
	if err != nil {
		t.Fatalf("acquire on free lock: %v", err)
	}
	if err := f1.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	f2, err := acquireObjectCacheLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := f2.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestAcquireObjectCacheLockWaitsThenWinsAfterRelease confirms a contended
// waiter still gets the lock (waits its turn) when the holder releases before
// the bound, rather than failing outright.
func TestAcquireObjectCacheLockWaitsThenWinsAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.lock")

	held, err := acquireObjectCacheLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	restore := setObjectCacheLockTimeoutForTest(5*time.Second, 10*time.Millisecond)
	defer restore()

	time.AfterFunc(150*time.Millisecond, func() { _ = held.Release() })

	f, err := acquireObjectCacheLock(path)
	if err != nil {
		t.Fatalf("waiter did not win the lock after release: %v", err)
	}
	_ = f.Release()
}

// TestAcquireObjectCacheLockNonHeldErrorPropagates confirms a non-contention
// error from the platform lock (e.g. an unopenable path) is returned as-is,
// not masked as a timeout.
func TestAcquireObjectCacheLockNonHeldErrorPropagates(t *testing.T) {
	// A path under a nonexistent directory: os.OpenFile fails with ENOENT,
	// which is not lock.ErrHeld.
	path := filepath.Join(t.TempDir(), "nonexistent-dir", "key.lock")

	_, err := acquireObjectCacheLock(path)
	if err == nil {
		t.Fatal("acquire succeeded against an unopenable path")
	}
	if errors.Is(err, ErrObjectCacheLockTimeout) {
		t.Fatalf("err = %v, want the underlying open error, not a timeout", err)
	}
	if errors.Is(err, lock.ErrHeld) {
		t.Fatalf("err = %v, want the underlying open error, not ErrHeld", err)
	}
}
