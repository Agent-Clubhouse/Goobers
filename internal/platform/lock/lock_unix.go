//go:build unix

package lock

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(file *os.File, nonBlocking bool) error {
	operation := syscall.LOCK_EX
	if nonBlocking {
		operation |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(file.Fd()), operation)
	if nonBlocking && errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrHeld
	}
	return err
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
