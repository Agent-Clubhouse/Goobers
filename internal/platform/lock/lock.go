package lock

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrHeld is returned by TryAcquire when another handle owns the lock.
var ErrHeld = errors.New("lock is held")

// Handle owns a shared or exclusive file lock. File returns the locked file and is valid
// until Release is called.
type Handle struct {
	mu         sync.Mutex
	file       *os.File
	holderPath string
}

// TryAcquire opens path and attempts to take an exclusive lock without waiting.
func TryAcquire(path string) (*Handle, error) {
	return acquire(path, true, os.O_CREATE|os.O_RDWR, false)
}

// TryAcquireExisting opens an existing path and attempts to take an exclusive
// lock without waiting. It never creates the lock file.
func TryAcquireExisting(path string) (*Handle, error) {
	return acquire(path, true, os.O_RDWR, false)
}

// TryAcquireExistingInRoot contains path resolution to root, including symlinks.
// It only locks an existing regular file and never creates or truncates one.
func TryAcquireExistingInRoot(root *os.Root, path string) (*Handle, error) {
	file, err := root.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("lock: existing lock must be a regular file")
	}
	if err == nil {
		err = lockFile(file, true, false)
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &Handle{file: file}, nil
}

// TryAcquireShared permits concurrent readers while excluding an exclusive
// holder. Like exclusive locks, it is released automatically on process exit.
func TryAcquireShared(path string) (*Handle, error) {
	return acquire(path, true, os.O_CREATE|os.O_RDWR, true)
}

func acquire(path string, nonBlocking bool, flags int, shared bool) (*Handle, error) {
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %q: %w", path, err)
	}
	if err := lockFile(file, nonBlocking, shared); err != nil {
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
	if h.holderPath != "" {
		// Cleared while still locked so it cannot delete the next holder's
		// record. Best effort: a leftover record is overwritten by the next
		// Announce and is only ever read by a waiter that found the lock held.
		_ = os.Remove(h.holderPath)
		h.holderPath = ""
	}
	return errors.Join(unlockFile(file), file.Close())
}
