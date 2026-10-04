package durability

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

type stubAtomicFile struct {
	name       string
	fail       AtomicWriteOperation
	written    []byte
	mode       fs.FileMode
	closeCall  int
	shortWrite bool
}

func (f *stubAtomicFile) Name() string { return f.name }

func (f *stubAtomicFile) Chmod(mode fs.FileMode) error {
	f.mode = mode
	if f.fail == AtomicWriteChmod {
		return errors.New("chmod failed")
	}
	return nil
}

func (f *stubAtomicFile) Write(data []byte) (int, error) {
	if f.fail == AtomicWriteWrite {
		return 0, errors.New("write failed")
	}
	if f.shortWrite {
		f.written = append(f.written, data[:len(data)-1]...)
		return len(data) - 1, nil
	}
	f.written = append(f.written, data...)
	return len(data), nil
}

func (f *stubAtomicFile) Sync() error {
	if f.fail == AtomicWriteSync {
		return errors.New("sync failed")
	}
	return nil
}

func (f *stubAtomicFile) Close() error {
	f.closeCall++
	if f.fail == AtomicWriteClose {
		return errors.New("close failed")
	}
	return nil
}

func TestReplaceFileReplacesExistingDestination(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "state.json.tmp")
	destination := filepath.Join(directory, "state.json")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(source, destination); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("destination = %q, want new content", got)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after replacement: %v", err)
	}
}

func TestWriteFileAtomicReplacesExistingDestinationWithMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o640); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestWriteFileAtomicFailurePathsCleanUp(t *testing.T) {
	failures := []AtomicWriteOperation{
		AtomicWriteChmod,
		AtomicWriteWrite,
		AtomicWriteSync,
		AtomicWriteClose,
		AtomicWriteReplace,
		AtomicWriteSyncDir,
	}
	for _, failure := range failures {
		t.Run(string(failure), func(t *testing.T) {
			file := &stubAtomicFile{name: "temporary", fail: failure}
			removed := false
			configure := func(config *atomicWriteConfig) {
				config.createTemp = func(_, _ string) (atomicFile, error) {
					return file, nil
				}
				config.remove = func(path string) error {
					if path != file.name {
						t.Fatalf("remove path = %q, want %q", path, file.name)
					}
					removed = true
					return nil
				}
				config.replace = func(_, _ string) error {
					if failure == AtomicWriteReplace {
						return errors.New("replace failed")
					}
					return nil
				}
				config.syncDir = func(string) error {
					if failure == AtomicWriteSyncDir {
						return errors.New("directory sync failed")
					}
					return nil
				}
			}
			err := WriteFileAtomic("destination", []byte("data"), 0o640, configure)
			var writeErr *AtomicWriteError
			if !errors.As(err, &writeErr) {
				t.Fatalf("error = %v, want AtomicWriteError", err)
			}
			if writeErr.Operation != failure {
				t.Fatalf("operation = %q, want %q", writeErr.Operation, failure)
			}
			if !removed {
				t.Fatal("temporary file was not cleaned up")
			}
			if failure != AtomicWriteClose && file.closeCall == 0 {
				t.Fatal("temporary file was not closed")
			}
		})
	}
}

func TestWriteFileAtomicCreateFailure(t *testing.T) {
	want := errors.New("create failed")
	err := WriteFileAtomic("destination", nil, 0o600, func(config *atomicWriteConfig) {
		config.createTemp = func(_, _ string) (atomicFile, error) {
			return nil, want
		}
	})
	var writeErr *AtomicWriteError
	if !errors.As(err, &writeErr) || writeErr.Operation != AtomicWriteCreateTemp {
		t.Fatalf("error = %v, want create AtomicWriteError", err)
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped create error", err)
	}
}

func TestWriteFileAtomicShortWriteCleansUp(t *testing.T) {
	file := &stubAtomicFile{name: "temporary", shortWrite: true}
	removed := false
	err := WriteFileAtomic("destination", []byte("data"), 0o600, func(config *atomicWriteConfig) {
		config.createTemp = func(_, _ string) (atomicFile, error) {
			return file, nil
		}
		config.remove = func(path string) error {
			if path != file.name {
				t.Fatalf("remove path = %q, want %q", path, file.name)
			}
			removed = true
			return nil
		}
	})
	var writeErr *AtomicWriteError
	if !errors.As(err, &writeErr) || writeErr.Operation != AtomicWriteWrite {
		t.Fatalf("error = %v, want write AtomicWriteError", err)
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want wrapped io.ErrShortWrite", err)
	}
	if file.closeCall != 1 {
		t.Fatalf("close calls = %d, want 1", file.closeCall)
	}
	if !removed {
		t.Fatal("temporary file was not cleaned up")
	}
}

func TestWriteFileAtomicPublishRaceCheck(t *testing.T) {
	file := &stubAtomicFile{name: "temporary"}
	raceChecked := false
	err := WriteFileAtomic("destination", []byte("data"), 0o600,
		WithPublishRaceCheck(func(path string) error {
			raceChecked = path == "destination"
			return nil
		}),
		func(config *atomicWriteConfig) {
			config.createTemp = func(_, _ string) (atomicFile, error) {
				return file, nil
			}
			config.remove = func(string) error { return nil }
			config.replace = func(_, _ string) error { return errors.New("lost race") }
		},
	)
	if err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if !raceChecked {
		t.Fatal("publish race was not checked")
	}
}

func TestWriteFileAtomicPublishRaceDoesNotReplaceExistingDestination(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "write-once")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o600,
		WithPublishRaceCheck(func(path string) error {
			_, err := os.Stat(path)
			return err
		})); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("content = %q, want first publication preserved", got)
	}
}

func TestMoveRenamesDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "legacy")
	destination := filepath.Join(root, "scoped")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "state"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Move(source, destination); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "state")); err != nil {
		t.Fatalf("moved content: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after move: %v", err)
	}
}

// RemoveFile is the delete half of the atomic-write protocol (#3562): the
// write path has waited out transient Windows contention since it was written
// and the delete path had no such retry, so a file this package had just
// published could refuse to be deleted and the caller treated that as final.
// The behaviour asserted here is the part that is the same everywhere — the
// file goes away, and an absent file reports ErrNotExist rather than success —
// so a platform that grew a retry cannot quietly change what the call MEANS.
func TestRemoveFileDeletesAndReportsAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stop-request")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFile(path); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after RemoveFile = %v, want ErrNotExist", err)
	}
	if err := RemoveFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RemoveFile on an absent path = %v, want ErrNotExist — callers "+
			"distinguish \"already consumed\" from \"could not consume\"", err)
	}
}

func TestWriteFileAtomicBestEffortModeTolerance(t *testing.T) {
	for _, errno := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		inject := WithChmod(func(string, fs.FileMode) error { return &os.PathError{Op: "chmod", Err: errno} })
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		// Default (fail closed): error reported as a chmod failure, nothing published.
		err := WriteFileAtomic(path, []byte("x"), 0o600, inject)
		var we *AtomicWriteError
		if !errors.As(err, &we) || we.Operation != AtomicWriteChmod {
			t.Fatalf("%v: default err = %v, want chmod failure", errno, err)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%v: file published despite chmod failure", errno)
		}
		// Opt-in best effort: succeeds.
		if err := WriteFileAtomic(path, []byte("x"), 0o600, inject, WithBestEffortMode()); err != nil {
			t.Fatalf("%v: best-effort err = %v", errno, err)
		}
	}
	// Other chmod errors still fail even when best effort.
	other := WithChmod(func(string, fs.FileMode) error { return syscall.EIO })
	err := WriteFileAtomic(filepath.Join(t.TempDir(), "f"), []byte("x"), 0o600, other, WithBestEffortMode())
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("EIO err = %v, want EIO", err)
	}
}
