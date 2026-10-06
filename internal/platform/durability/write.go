package durability

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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
	bestEffortMode   bool
	chmod            func(path string, mode fs.FileMode) error
	link             func(oldname, newname string) error
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

// WithBestEffortMode makes a chmod failure that means the filesystem has no
// permission bits to set (EPERM, ENOTSUP, EOPNOTSUPP, as returned by CIFS/SMB
// mounts with nounix) non-fatal. The staged file keeps the owner-only mode
// os.CreateTemp gave it. Only callers whose data is not secret-bearing and
// that must work on such mounts should opt in; by default a mode failure
// fails closed.
func WithBestEffortMode() Option {
	return func(config *atomicWriteConfig) {
		config.bestEffortMode = true
	}
}

// WithChmod overrides how the requested mode is applied to the staged file
// (by temp path). It exists mainly as a fault-injection seam.
func WithChmod(chmod func(path string, mode fs.FileMode) error) Option {
	return func(config *atomicWriteConfig) {
		config.chmod = chmod
	}
}

// isModeUnsupported reports whether err means the filesystem cannot apply
// permission bits at all.
func isModeUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP)
}

// WithPublishRaceCheck publishes without replacing an existing target and
// treats a lost race as successful when check verifies the target.
func WithPublishRaceCheck(check func(path string) error) Option {
	return func(config *atomicWriteConfig) {
		config.publishRaceCheck = check
		config.replace = func(source, destination string) error {
			link := config.link
			if link == nil {
				link = os.Link
			}
			return publishNoClobber(link, source, destination)
		}
	}
}

// WithLink overrides the hard-link primitive used by the no-clobber publish
// (WithPublishRaceCheck). It exists mainly as a fault-injection seam.
func WithLink(link func(oldname, newname string) error) Option {
	return func(config *atomicWriteConfig) {
		config.link = link
	}
}

// isLinkUnsupported reports whether err means the filesystem or mount cannot
// create hard links (CIFS/SMB with nounix returns EPERM or ENOTSUP).
func isLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EXDEV)
}

// publishNoClobber publishes source at destination without replacing an
// existing destination. It prefers an atomic hard link. On filesystems without
// hard links it falls back to stat-then-rename: an existing destination is
// reported as fs.ErrExist (so the race check decides), otherwise the staged
// file is renamed into place. That fallback is not atomic against a concurrent
// publisher that creates the destination between the stat and the rename; the
// later rename wins. Callers whose payloads are content-addressed are
// unaffected.
func publishNoClobber(link func(string, string) error, source, destination string) error {
	err := link(source, destination)
	if err == nil || !isLinkUnsupported(err) {
		return err
	}
	if _, statErr := os.Lstat(destination); statErr == nil {
		return fs.ErrExist
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	return os.Rename(source, destination)
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

	var chmodErr error
	if config.chmod != nil {
		chmodErr = config.chmod(tempPath, mode)
	} else {
		chmodErr = file.Chmod(mode)
	}
	if chmodErr != nil && config.bestEffortMode && isModeUnsupported(chmodErr) {
		chmodErr = nil
	}
	if err := chmodErr; err != nil {
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
