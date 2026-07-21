package lock

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrHeld is returned by TryAcquire when another handle owns the lock.
var ErrHeld = errors.New("lock is held")

// Handle owns an exclusive file lock. File returns the locked file and is valid
// until Release is called.
type Handle struct {
	mu   sync.Mutex
	file *os.File
}

// Acquire opens path and waits until it holds an exclusive lock.
func Acquire(path string) (*Handle, error) {
	return acquire(path, false)
}

// TryAcquire opens path and attempts to take an exclusive lock without waiting.
func TryAcquire(path string) (*Handle, error) {
	return acquire(path, true)
}

func acquire(path string, nonBlocking bool) (*Handle, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
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
