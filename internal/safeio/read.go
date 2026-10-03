// Package safeio provides bounded file reads within safe filesystem roots.
package safeio

import (
	"errors"
	"io"
	"math"
	"os"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// ErrLimitExceeded reports a regular file larger than the requested read limit.
var ErrLimitExceeded = errors.New("safeio: file exceeds read limit")

// ErrLimitExceededDuringRead reports a file that grew after its size check.
var ErrLimitExceededDuringRead = errors.New("safeio: file grew beyond read limit")

// ReadRegularInRoot reads a regular file without allowing it to escape dir.
func ReadRegularInRoot(dir, rel string, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, ErrLimitExceeded
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := safeopen.OpenRegularInRoot(root, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return readRegular(file, info, limit)
}

func readRegular(file io.Reader, info os.FileInfo, limit int64) ([]byte, error) {
	if !info.Mode().IsRegular() {
		return nil, safeopen.ErrNotRegular
	}
	if info.Size() > limit {
		return nil, ErrLimitExceeded
	}
	readLimit := limit
	if limit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrLimitExceededDuringRead
	}
	return data, nil
}
