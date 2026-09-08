// Package mutationsidecar bounds reads of the stage's provider receipt handoff.
package mutationsidecar

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// MaxBytes caps memory used by a single stage's receipt handoff. Oversized
// files are rejected, never truncated into an apparently complete receipt set.
const MaxBytes = 16 << 20

// MaxLines bounds the number of decoded facts or per-line diagnostics.
const MaxLines = 10000

// Read reads a complete bounded regular-file handoff, refusing symlink leaves.
// Missing files retain os.IsNotExist compatibility for existing stage callers.
func Read(workspace string) (data []byte, err error) {
	dir, err := safeopen.Open(workspace)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	f, err := safeopen.OpenAt(dir, "mutations.jsonl")
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mutation sidecar must be a regular file (mode %s)", info.Mode()&os.ModeType)
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("mutation sidecar exceeds %d bytes", MaxBytes)
	}
	data, err = io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("mutation sidecar exceeds %d bytes", MaxBytes)
	}
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	if lines > MaxLines {
		return nil, fmt.Errorf("mutation sidecar exceeds %d lines", MaxLines)
	}
	return data, nil
}
