package durability

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// AtomicWriteOperation identifies the step that failed during WriteFileAtomic.
type AtomicWriteOperation string

// Atomic write operations identify each durability step for error handling.
const (
	AtomicWriteCreateTemp AtomicWriteOperation = "create temporary file"
	AtomicWriteChmod      AtomicWriteOperation = "set temporary file mode"
	AtomicWriteWrite      AtomicWriteOperation = "write temporary file"
	AtomicWriteSync       AtomicWriteOperation = "sync temporary file"
	AtomicWriteClose      AtomicWriteOperation = "close temporary file"
	AtomicWriteReplace    AtomicWriteOperation = "replace destination"
	AtomicWriteSyncDir    AtomicWriteOperation = "sync destination directory"
)

// AtomicWriteError reports which atomic-write step failed while preserving the
// underlying error text and identity.
type AtomicWriteError struct {
	Operation AtomicWriteOperation
	Err       error
}

func (e *AtomicWriteError) Error() string { return e.Err.Error() }
func (e *AtomicWriteError) Unwrap() error { return e.Err }

type atomicFile interface {
	Name() string
	Chmod(fs.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type atomicWriteConfig struct {
	tempPattern      string
	publishRaceCheck func(string) error
	createTemp       func(string, string) (atomicFile, error)
	remove           func(string) error
	replace          func(string, string) error
	syncDir          func(string) error
}

// Option configures WriteFileAtomic.
type Option func(*atomicWriteConfig)

// WithTempPattern overrides the temporary file pattern used in the
// destination directory.
func WithTempPattern(pattern string) Option {
	return func(config *atomicWriteConfig) {
		config.tempPattern = pattern
	}
}

// WithPublishRaceCheck publishes without replacing an existing target and
// treats a lost race as successful when check verifies the target.
func WithPublishRaceCheck(check func(path string) error) Option {
	return func(config *atomicWriteConfig) {
		config.publishRaceCheck = check
		config.replace = os.Link
	}
}

// WriteFileAtomic writes data to a sibling temporary file and atomically
// replaces path after the file contents have been flushed.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode, opts ...Option) error {
	config := atomicWriteConfig{
		tempPattern: "." + filepath.Base(path) + ".tmp-*",
		createTemp: func(directory, pattern string) (atomicFile, error) {
			return os.CreateTemp(directory, pattern)
		},
		remove:  os.Remove,
		replace: ReplaceFile,
		syncDir: SyncDir,
	}
	for _, opt := range opts {
		opt(&config)
	}

	directory := filepath.Dir(path)
	file, err := config.createTemp(directory, config.tempPattern)
	if err != nil {
		return atomicWriteError(AtomicWriteCreateTemp, err)
	}
	tempPath := file.Name()
	defer func() { _ = config.remove(tempPath) }()

	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return atomicWriteError(AtomicWriteChmod, err)
	}
	if written, err := file.Write(data); err != nil {
		_ = file.Close()
		return atomicWriteError(AtomicWriteWrite, err)
	} else if written != len(data) {
		_ = file.Close()
		return atomicWriteError(AtomicWriteWrite, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return atomicWriteError(AtomicWriteSync, err)
	}
	if err := file.Close(); err != nil {
		return atomicWriteError(AtomicWriteClose, err)
	}
	if err := config.replace(tempPath, path); err != nil {
		if config.publishRaceCheck != nil && config.publishRaceCheck(path) == nil {
			return nil
		}
		return atomicWriteError(AtomicWriteReplace, err)
	}
	if err := config.syncDir(directory); err != nil {
		return atomicWriteError(AtomicWriteSyncDir, err)
	}
	return nil
}

func atomicWriteError(operation AtomicWriteOperation, err error) error {
	return &AtomicWriteError{Operation: operation, Err: err}
}
