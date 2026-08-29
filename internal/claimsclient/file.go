package claimsclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
)

// FileConfig describes the instance's own claim ledger.
type FileConfig struct {
	// LedgerPath is claims.json under the instance's scheduler dir.
	LedgerPath string
	// Lock serializes a critical section against every other process touching
	// the ledger — cmd/goobers's withClaimLock over claims.lock, with its
	// operation label, timeout, and slow-lock journaling. Nil means the caller
	// already holds the lock (it is inside its own withClaimLock*), so the
	// backend opens the ledger and operates without acquiring anything.
	Lock func(operation string, fn func() error) error
	// MergeLock is the instance-wide merge flock (cmd/goobers's withFileLock
	// over merge.lock). Nil refuses MergeLock.
	MergeLock func(fn func() error) error
	// Options configure every ledger open (instance log, clock).
	Options []localscheduler.LedgerOption
	// Open overrides the ledger open (tests inject fault-injecting ledgers);
	// nil opens localscheduler.OpenClaimLedger.
	Open func(path string, opts ...localscheduler.LedgerOption) (FileLedger, error)
}

// FileLedger is the slice of *localscheduler.ClaimLedger the file backend
// operates on — seam-shaped so tests can wrap the real ledger.
type FileLedger interface {
	Claim(itemID, runID, workflow string, leaseDuration time.Duration) (bool, string, error)
	ClaimScoped(key Key, runID, workflow string, leaseDuration time.Duration) (bool, string, error)
	Release(itemID, runID string) error
	ReleaseScoped(key Key, runID string) error
	ReleaseEntry(entry Entry, runID string) error
	ForRunAll(runID string) []Entry
	Snapshot() []Entry
	HistorySnapshot() []Entry
}

// Default operation labels for a bare mutating primitive called outside
// Locked. Every cmd/goobers seam wraps its mutations in Locked with its own
// historical label; these exist so a bare call still serializes.
const (
	fileOperationClaim      = "claims.client.claim"
	fileOperationRelease    = "claims.client.release"
	fileOperationReleaseAll = "claims.client.release-all"
)

// File is the file-ledger backend.
type File struct {
	cfg FileConfig
}

// NewFile constructs the file backend.
func NewFile(cfg FileConfig) (*File, error) {
	if cfg.LedgerPath == "" {
		return nil, errors.New("claimsclient: file ledger path is required")
	}
	if cfg.Open == nil {
		cfg.Open = func(path string, opts ...localscheduler.LedgerOption) (FileLedger, error) {
			return localscheduler.OpenClaimLedger(path, opts...)
		}
	}
	return &File{cfg: cfg}, nil
}

func (f *File) open() (*fileSession, error) {
	ledger, err := f.cfg.Open(f.cfg.LedgerPath, f.cfg.Options...)
	if err != nil {
		return nil, fmt.Errorf("open claim ledger: %w", err)
	}
	return &fileSession{ledger: ledger, file: f}, nil
}

// Locked implements Ledger: one lock acquisition, one fresh open, fn.
func (f *File) Locked(_ context.Context, operation string, fn func(Ledger) error) error {
	run := func() error {
		session, err := f.open()
		if err != nil {
			return err
		}
		return fn(session)
	}
	if f.cfg.Lock == nil {
		return run()
	}
	return f.cfg.Lock(operation, run)
}

// ClaimScoped implements Ledger under a default-labelled critical section.
func (f *File) ClaimScoped(ctx context.Context, key Key, runID, workflow string, lease time.Duration) (ok bool, holder string, err error) {
	err = f.Locked(ctx, fileOperationClaim, func(session Ledger) error {
		var claimErr error
		ok, holder, claimErr = session.ClaimScoped(ctx, key, runID, workflow, lease)
		return claimErr
	})
	return ok, holder, err
}

// ReleaseScoped implements Ledger under a default-labelled critical section.
func (f *File) ReleaseScoped(ctx context.Context, key Key, runID string) error {
	return f.Locked(ctx, fileOperationRelease, func(session Ledger) error {
		return session.ReleaseScoped(ctx, key, runID)
	})
}

// ReleaseAllForRun implements Ledger under a default-labelled critical section.
func (f *File) ReleaseAllForRun(ctx context.Context, runID string) (released []Entry, err error) {
	err = f.Locked(ctx, fileOperationReleaseAll, func(session Ledger) error {
		var releaseErr error
		released, releaseErr = session.ReleaseAllForRun(ctx, runID)
		return releaseErr
	})
	return released, err
}

// ForRunAll implements Ledger as an unlocked fresh read — the discipline the
// read-only CLI consumers (backlog-dedupe, backlog-health) had against the
// ledger file; callers that want the read inside a critical section wrap it
// in Locked.
func (f *File) ForRunAll(ctx context.Context, runID string) ([]Entry, error) {
	session, err := f.open()
	if err != nil {
		return nil, err
	}
	return session.ForRunAll(ctx, runID)
}

// ListNamespace implements Ledger as an unlocked fresh read (see ForRunAll).
func (f *File) ListNamespace(ctx context.Context, gaggle, provider string) (Listing, error) {
	session, err := f.open()
	if err != nil {
		return Listing{}, err
	}
	return session.ListNamespace(ctx, gaggle, provider)
}

// MergeLock implements Ledger over the instance-wide merge flock. The key is
// deliberately unused: the flock is one per instance (#719's shape), and the
// per-repository lease is the plane's refinement, not the file's. There is no
// renewal race to fail closed on here — the flock is held for fn's whole
// duration, not leased and polled — so fn simply gets ctx back unchanged.
func (f *File) MergeLock(ctx context.Context, _ MergeLock, fn func(context.Context) error) error {
	if f.cfg.MergeLock == nil {
		return errors.New("claimsclient: file backend has no merge lock configured")
	}
	return f.cfg.MergeLock(func() error { return fn(ctx) })
}

// fileSession is one open ledger inside a held critical section. Its
// primitives never lock or reopen, so a nested Locked is re-entrant rather
// than a self-deadlock on the flock.
type fileSession struct {
	ledger FileLedger
	file   *File
}

func (s *fileSession) Locked(_ context.Context, _ string, fn func(Ledger) error) error {
	return fn(s)
}

func (s *fileSession) ClaimScoped(_ context.Context, key Key, runID, workflow string, lease time.Duration) (bool, string, error) {
	if key.Gaggle == "" && key.Provider == "" {
		return s.ledger.Claim(key.ExternalID, runID, workflow, lease)
	}
	return s.ledger.ClaimScoped(key, runID, workflow, lease)
}

func (s *fileSession) ReleaseScoped(_ context.Context, key Key, runID string) error {
	if key.Gaggle == "" && key.Provider == "" {
		return s.ledger.Release(key.ExternalID, runID)
	}
	return s.ledger.ReleaseScoped(key, runID)
}

func (s *fileSession) ReleaseAllForRun(_ context.Context, runID string) ([]Entry, error) {
	entries := s.ledger.ForRunAll(runID)
	released := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if err := s.ledger.ReleaseEntry(entry, runID); err != nil {
			return released, fmt.Errorf("release claim %s for run %s: %w", entry.ItemID, runID, err)
		}
		released = append(released, entry)
	}
	return released, nil
}

func (s *fileSession) ForRunAll(_ context.Context, runID string) ([]Entry, error) {
	return s.ledger.ForRunAll(runID), nil
}

func (s *fileSession) ListNamespace(_ context.Context, gaggle, provider string) (Listing, error) {
	var listing Listing
	for _, entry := range s.ledger.Snapshot() {
		if InNamespace(entry, gaggle, provider) {
			listing.Entries = append(listing.Entries, entry)
		}
	}
	for _, entry := range s.ledger.HistorySnapshot() {
		if InNamespace(entry, gaggle, provider) {
			listing.History = append(listing.History, entry)
		}
	}
	return listing, nil
}

func (s *fileSession) MergeLock(ctx context.Context, lock MergeLock, fn func(context.Context) error) error {
	return s.file.MergeLock(ctx, lock, fn)
}
