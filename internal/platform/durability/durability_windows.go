//go:build windows

package durability

import (
	"errors"
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
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) &&
			!errors.Is(err, windows.ERROR_SHARING_VIOLATION) &&
			!errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if time.Now().After(deadline) {
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
