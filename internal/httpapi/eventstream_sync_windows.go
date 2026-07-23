//go:build windows

package httpapi

import "os"

func syncObservedFile(reader *os.File, path string) error {
	// FlushFileBuffers requires a write-capable handle on Windows.
	if err := reader.Close(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}
