//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const lockedBytesLow = 1

func lockFile(file *os.File, nonBlocking bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		flags,
		0,
		lockedBytesLow,
		0,
		&windows.Overlapped{},
	)
	if nonBlocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrHeld
	}
	return err
}

func unlockFile(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		lockedBytesLow,
		0,
		&windows.Overlapped{},
	)
}
