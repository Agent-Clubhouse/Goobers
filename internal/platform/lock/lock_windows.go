//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const (
	lockByteOffsetHigh = 1
	lockedBytesLow     = 1
)

func lockFile(file *os.File, nonBlocking bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	// Keep the mandatory lock range away from state stored at the start of
	// daemon lock files. Windows permits locking ranges beyond end-of-file.
	overlapped := windows.Overlapped{OffsetHigh: lockByteOffsetHigh}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		flags,
		0,
		lockedBytesLow,
		0,
		&overlapped,
	)
	if nonBlocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrHeld
	}
	return err
}

func unlockFile(file *os.File) error {
	overlapped := windows.Overlapped{OffsetHigh: lockByteOffsetHigh}
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		lockedBytesLow,
		0,
		&overlapped,
	)
}
