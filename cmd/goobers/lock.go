package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// acquireInstanceLock takes a non-blocking exclusive flock on lockPath so a
// second `goobers up` on the same instance root fails fast with a clear
// message (issue #23 AC3) instead of two daemons racing the same
// runs/scheduler state. The returned release func unlocks and closes the
// file; call it (typically via defer) when the holder exits.
func acquireInstanceLock(lockPath string) (release func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another `goobers up` already holds the lock on this instance root (%s)", lockPath)
		}
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
