//go:build windows

package durability

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const replaceRetryWindow = 2 * time.Second

// ReplaceFile atomically replaces destination with source and requests durable metadata.
func ReplaceFile(source, destination string) error {
	return movePath(source, destination, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Move atomically renames a path when the destination does not exist.
func Move(source, destination string) error {
	return movePath(source, destination, windows.MOVEFILE_WRITE_THROUGH)
}

// RemoveFile deletes path, retrying the transient Windows failures that a file
// this package just wrote can still be holding.
//
// os.Remove has no such retry and the write path has had one since it was
// written, which is the asymmetry #3562 reported: a file published by
// ReplaceFile (temp + MoveFileEx) can still be briefly undeletable — a scanner,
// an indexer, or the filesystem filter stack holds a handle opened without
// FILE_SHARE_DELETE — and Windows answers the delete with
// ERROR_SHARING_VIOLATION rather than deleting it. Nothing about that condition
// is permanent; it just is not over yet.
//
// The consequence is not confined to tests. The daemon's stop-request watch
// (cmd/goobers/up.go) treats a failure from ConsumeStopRequest as terminal and
// stops watching, so one transient sharing violation left a live Windows daemon
// unable to be asked to drain for the rest of its life.
//
// ErrNotExist returns immediately: an absent file is an answer, not a
// contention, and every caller here distinguishes the two.
func RemoveFile(path string) error {
	deadline := time.Now().Add(replaceRetryWindow)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !transientFileError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// transientFileError reports whether err is one of the Windows contention
// codes that another handle on the same file produces and that goes away on
// its own. Shared by the move and remove retries so the two cannot come to
// disagree about which failures are worth waiting out.
func transientFileError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func movePath(source, destination string, flags uint32) error {
	source, err := extendedLengthPath(source)
	if err != nil {
		return err
	}
	destination, err = extendedLengthPath(destination)
	if err != nil {
		return err
	}
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(replaceRetryWindow)
	for {
		err = windows.MoveFileEx(sourcePath, destinationPath, flags)
		if err == nil {
			return nil
		}
		if !transientFileError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func extendedLengthPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if strings.HasPrefix(absolute, `\\?\`) {
		return absolute, nil
	}
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`), nil
	}
	return `\\?\` + absolute, nil
}

// SyncDir is a no-op on Windows, where FlushFileBuffers cannot flush a
// directory through the read-only handle returned by os.Open.
func SyncDir(string) error {
	return nil
}
