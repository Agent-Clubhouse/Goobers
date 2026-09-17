package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrHeld is returned by TryAcquire when another handle owns the lock.
var ErrHeld = errors.New("lock is held")

// Handle owns an exclusive file lock. File returns the locked file and is valid
// until Release is called.
type Handle struct {
	mu   sync.Mutex
	file *os.File
}

// TryAcquire opens path and attempts to take an exclusive lock without waiting.
func TryAcquire(path string) (*Handle, error) {
	return acquire(path, true, os.O_CREATE|os.O_RDWR)
}

// TryAcquireExisting opens an existing path and attempts to take an exclusive
// lock without waiting. It never creates the lock file.
func TryAcquireExisting(path string) (*Handle, error) {
	return acquire(path, true, os.O_RDWR)
}

// AcquireWithin opens path and retries an exclusive lock until it is taken, the
// context ends, or wait elapses. It returns the LAST attempt's error (wrapping
// ErrHeld when the holder simply never let go), so a caller that gives up
// reports contention rather than a bare deadline.
//
// This exists because "try once, then fail the whole operation" is the wrong
// posture for a lock guarding a SHARED resource that many short operations
// touch. On the goobernetes cloud instance the recovery inventory lock is taken
// by per-run cleanups, terminal renewals and the periodic sweep; a single
// missed acquisition aborted a worktree finalize, which left a stalled run
// un-terminalizable, which wedged a maxConcurrentRuns:1 lane permanently
// (#5272). The holders are all brief, so a bounded wait converts almost all of
// those failures into completed work while still refusing to block forever.
func AcquireWithin(ctx context.Context, path string, wait time.Duration) (*Handle, error) {
	deadline := time.Now().Add(wait)
	// Backoff starts short because the common case is a collision between two
	// operations measured in milliseconds, and caps so a long holder does not
	// turn into a long spin.
	const (
		initialBackoff = 2 * time.Millisecond
		maxBackoff     = 100 * time.Millisecond
	)
	backoff := initialBackoff
	for {
		handle, err := acquire(path, true, os.O_CREATE|os.O_RDWR)
		if err == nil {
			return handle, nil
		}
		if !errors.Is(err, ErrHeld) {
			// Not contention: a missing directory or a permission problem will
			// not resolve by waiting.
			return nil, err
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			return nil, err
		} else if backoff > remaining {
			backoff = remaining
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func acquire(path string, nonBlocking bool, flags int) (*Handle, error) {
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %q: %w", path, err)
	}
	if err := lockFile(file, nonBlocking); err != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("lock: acquire %q: %w", path, err), closeErr)
	}
	return &Handle{file: file}, nil
}

// File returns the file descriptor that owns the lock. The caller must not
// close it or use it concurrently with Release.
func (h *Handle) File() *os.File {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.file
}

// Release unlocks and closes the lock file. It is safe to call more than once.
func (h *Handle) Release() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return nil
	}
	file := h.file
	h.file = nil
	return errors.Join(unlockFile(file), file.Close())
}
