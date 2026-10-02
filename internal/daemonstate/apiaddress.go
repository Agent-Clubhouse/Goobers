package daemonstate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
)

// APIAddressFileName is the scheduler's published daemon API address file.
const APIAddressFileName = "api.address"

// daemonAPIAddressTempFile is the slice of *os.File PublishAPIAddress
// uses, so tests can inject write and close failures at the durability
// boundary (#4575).
type daemonAPIAddressTempFile interface {
	io.WriteCloser
	Name() string
}

// createDaemonAPIAddressTempFile is the temporary-file factory behind
// PublishAPIAddress; tests replace it to fail a write or close.
var createDaemonAPIAddressTempFile = func(dir, pattern string) (daemonAPIAddressTempFile, error) {
	return os.CreateTemp(dir, pattern)
}

// PublishAPIAddress atomically replaces the published daemon API address.
func PublishAPIAddress(path, address string) error {
	file, err := createDaemonAPIAddressTempFile(filepath.Dir(path), "."+APIAddressFileName+"-*")
	if err != nil {
		return fmt.Errorf("create daemon API address file: %w", err)
	}
	tempPath := file.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.WriteString(file, address+"\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write daemon API address file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close daemon API address file: %w", err)
	}
	if err := durability.ReplaceFile(tempPath, path); err != nil {
		return fmt.Errorf("publish daemon API address: %w", err)
	}
	removeTemp = false
	return nil
}

// RemoveAPIAddress removes the published address, tolerating an absent file.
func RemoveAPIAddress(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon API address: %w", err)
	}
	return nil
}
