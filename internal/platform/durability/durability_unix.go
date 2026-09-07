//go:build !windows

package durability

import (
	"errors"
	"os"
	"syscall"
)

// ReplaceFile atomically replaces destination with source.
func ReplaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

// Move atomically renames a path when the destination does not exist.
func Move(source, destination string) error {
	return os.Rename(source, destination)
}

// RemoveFile deletes path. The Windows implementation waits out the transient
// contention a just-written file can still be under (#3562); on unix a delete
// is an unlink against the directory entry and has no such state, so this is
// os.Remove and exists only so callers need not branch on the platform.
func RemoveFile(path string) error {
	return os.Remove(path)
}

// SyncDir flushes directory metadata after an atomic rename.
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil &&
		!errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
