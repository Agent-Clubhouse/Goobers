package stateclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// FileConfig configures the file backend: the instance's scheduler directory
// and the per-key cross-process lock the caller owns.
type FileConfig struct {
	// Dir is the instance scheduler directory the state files live in — the
	// SAME directory the daemon's own scheduler reads and writes, so a
	// plane-served write and an in-process write land on one file rather than
	// two copies. Each key resolves under it through KeyRelativePath, which is
	// the key's own name for all but the backlog-health cursor (whose prefix
	// resolves to the backlog-health/ subdirectory the ledger has always lived
	// in). The route's gaggle is a containment scope, not a second storage
	// namespace: these files are instance-wide today and stay so, or the
	// far-side evidence (a pod-executed backlog-query advancing the cursor the
	// daemon reads) could not hold.
	Dir string
	// Lock scopes one critical section for key, labelled operation. Supplied
	// by the caller so the key's EXISTING lock discipline is preserved
	// exactly: claims.lock for blocked.json and the scan cursors,
	// post-merge-reconcile.lock for the post-merge ledger,
	// sibling-context-cache.lock for the sibling cache.
	//
	// nil means the caller is ALREADY inside the key's lock (the
	// heldClaimLedger shape): taking it again on a non-reentrant flock would
	// wait on itself until the timeout, so the primitives run directly.
	Lock func(key, operation string, fn func() error) error
}

// File is the file backend: the instance's own scheduler-state files under
// their own locks.
type File struct {
	cfg FileConfig
}

// NewFile constructs the file backend.
func NewFile(cfg FileConfig) (*File, error) {
	if cfg.Dir == "" {
		return nil, errors.New("stateclient: file backend requires a scheduler directory")
	}
	return &File{cfg: cfg}, nil
}

// path resolves key to its file. checkKey has already refused everything
// outside the closed namespace, so the join cannot escape Dir — the guard runs
// BEFORE the join for exactly that reason, inside KeyRelativePath.
func (f *File) path(key string) (string, error) {
	relative, err := KeyRelativePath(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(f.cfg.Dir, relative), nil
}

func (f *File) locked(key, operation string, fn func() error) error {
	if f.cfg.Lock == nil {
		return fn()
	}
	return f.cfg.Lock(key, operation, fn)
}

// readValue loads key's current bytes and ETag. An absent file is the zero
// Value, exactly as every one of these files' own loader treats a missing file
// as its empty state.
func (f *File) readValue(key string) (Value, error) {
	path, err := f.path(key)
	if err != nil {
		return Value{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Value{}, nil
	}
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: read %s: %w", path, err)
	}
	return Value{Data: data, ETag: ETagFor(data)}, nil
}

// writeValue replaces key's bytes atomically (write-then-rename, the same
// torn-write guard the claim ledger and blocked.json already use).
func (f *File) writeValue(key string, data []byte) (Value, error) {
	path, err := f.path(key)
	if err != nil {
		return Value{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Value{}, fmt.Errorf("stateclient: create scheduler directory: %w", err)
	}
	if err := journal.WriteFileAtomic(path, data, 0o644); err != nil {
		return Value{}, fmt.Errorf("stateclient: write %s: %w", path, err)
	}
	return Value{Data: data, ETag: ETagFor(data)}, nil
}

// Get implements Store.
func (f *File) Get(_ context.Context, key string) (Value, error) {
	if err := checkKey(key); err != nil {
		return Value{}, err
	}
	var value Value
	err := f.locked(key, "state.get."+key, func() error {
		var readErr error
		value, readErr = f.readValue(key)
		return readErr
	})
	return value, err
}

// Put implements Store: the compare-and-swap, served under the key's lock so
// the compare and the swap are one operation against every other writer —
// including a plane caller, which reaches this same code through the daemon.
func (f *File) Put(_ context.Context, key string, data []byte, ifMatch string) (Value, error) {
	if err := checkKey(key); err != nil {
		return Value{}, err
	}
	if err := checkValue(data); err != nil {
		return Value{}, err
	}
	var written Value
	err := f.locked(key, "state.put."+key, func() error {
		current, readErr := f.readValue(key)
		if readErr != nil {
			return readErr
		}
		if current.ETag != ifMatch {
			return ErrPreconditionFailed
		}
		var writeErr error
		written, writeErr = f.writeValue(key, data)
		return writeErr
	})
	return written, err
}

// Section implements Store: the key's lock, held across the whole of fn.
func (f *File) Section(_ context.Context, key, operation string, fn func() error) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return f.locked(key, operation, fn)
}

// Update implements Store: read-modify-write inside ONE lock acquisition, so
// the compare-and-swap can never lose and fn runs exactly once. This is the
// critical section today's in-process callers already hold.
func (f *File) Update(_ context.Context, key, operation string, fn func(Value) ([]byte, bool, error)) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return f.locked(key, operation, func() error {
		current, err := f.readValue(key)
		if err != nil {
			return err
		}
		data, write, err := fn(current)
		if err != nil || !write {
			return err
		}
		if err := checkValue(data); err != nil {
			return err
		}
		_, err = f.writeValue(key, data)
		return err
	})
}
