package claimsclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
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
	// MergeLockPoll controls how often a local merge retries after the durable
	// ledger reports a live plane merge lease. Zero uses the default.
	MergeLockPoll time.Duration
	// ProviderReleaseLock serializes cleanup attempts for one item, separately
	// from the claims lock. It must be cross-process and held across I/O.
	ProviderReleaseLock func(Key, func() error) error
	// Options configure every ledger open (instance log, clock).
	Options []localscheduler.LedgerOption
	// Open overrides the ledger open (tests inject fault-injecting ledgers);
	// nil opens localscheduler.OpenClaimLedger.
	Open func(path string, opts ...localscheduler.LedgerOption) (FileLedger, error)
	// Shared resolves the trusted workflow policy and repository binding.
	// Nil retains the local-only assembly; shared-enabled assemblies must
	// install this resolver. Shared mutations are forbidden in Locked sessions;
	// use the outer File primitives or Sequence so provider I/O stays outside.
	Shared SharedClaimResolver
}

// FileLedger is the slice of *localscheduler.ClaimLedger the file backend
// operates on — seam-shaped so tests can wrap the real ledger.
type FileLedger interface {
	RecordClaimVerification(Entry, localscheduler.ClaimVerification) (bool, error)
	Claim(itemID, runID, workflow string, leaseDuration time.Duration) (bool, string, error)
	ClaimScoped(key Key, runID, workflow string, leaseDuration time.Duration) (bool, string, error)
	Release(itemID, runID string, reason claimrelease.Reason) error
	ReleaseScoped(key Key, runID string, reason claimrelease.Reason) error
	ReleaseEntry(entry Entry, runID string, reason claimrelease.Reason) error
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
	fileOperationMergeCheck = "claims.client.merge-check"
)

const defaultFileMergeLockPoll = 2 * time.Second

var errFileMergeLeaseHeld = errors.New("claimsclient: live plane merge lease")

// File is the file-ledger backend.
type File struct {
	cfg       FileConfig
	operation string
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
	if cfg.MergeLockPoll <= 0 {
		cfg.MergeLockPoll = defaultFileMergeLockPoll
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
	if f.cfg.Shared != nil && !localscheduler.IsPublicationClaimKey(key.ExternalID) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if f.cfg.Lock == nil {
			return false, "", ErrSharedRequiresUnlocked
		}
		binding, resolveErr := f.cfg.Shared.Admission(ctx, key, runID, workflow)
		if resolveErr != nil {
			return false, "", resolveErr
		}
		if binding != nil {
			return f.claimShared(ctx, key, runID, workflow, lease, *binding, nil)
		}
	}
	err = f.Locked(ctx, f.operationName(fileOperationClaim), func(session Ledger) error {
		var claimErr error
		ok, holder, claimErr = session.(*fileSession).claimLocal(key, runID, workflow, lease)
		return claimErr
	})
	return ok, holder, err
}

// ReleaseScoped implements Ledger under a default-labelled critical section.
func (f *File) ReleaseScoped(ctx context.Context, key Key, runID string, reason claimrelease.Reason) error {
	var shared *Entry
	err := f.Locked(ctx, f.operationName(fileOperationRelease), func(session Ledger) error {
		held := session.(*fileSession)
		for _, entry := range held.ledger.ForRunAll(runID) {
			if KeyForEntry(entry) == key && !entry.SharedDeadline.IsZero() {
				shared = &entry
				return nil
			}
		}
		return held.ReleaseScoped(ctx, key, runID, reason)
	})
	if err != nil || shared == nil {
		return err
	}
	_, err = f.releaseShared(ctx, *shared, reason, time.Time{})
	return err
}

// ReleaseAllForRun snapshots under the claims lock and releases each exact
// incarnation separately, allowing shared provider I/O between short sections.
func (f *File) ReleaseAllForRun(ctx context.Context, runID string, reason claimrelease.Reason) (released []Entry, err error) {
	var entries []Entry
	err = f.Locked(ctx, f.operationName(fileOperationReleaseAll), func(session Ledger) error {
		entries = session.(*fileSession).ledger.ForRunAll(runID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsPublicationClaim() {
			continue
		}
		didRelease, err := f.ReleaseExpected(ctx, entry, reason)
		if err != nil {
			return released, err
		}
		if didRelease {
			released = append(released, reportEntry(entry))
		}
	}
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

// MergeLock implements Ledger over the instance-wide merge flock. After taking
// that flock it reads the durable claim ledger under claims.lock, preserving
// merge.lock -> claims.lock ordering. This second check keeps a local merge out
// while an unexpired plane lease survives a daemon crash, when the daemon-owned
// flock handle has disappeared but the delegated merge window remains live.
func (f *File) MergeLock(ctx context.Context, _ MergeLock, fn func(context.Context) error) error {
	if f.cfg.MergeLock == nil {
		return errors.New("claimsclient: file backend has no merge lock configured")
	}
	holder := ""
	for {
		err := f.cfg.MergeLock(func() error {
			var checkErr error
			holder, checkErr = f.livePlaneMergeLease()
			if checkErr != nil {
				return checkErr
			}
			if holder != "" {
				return errFileMergeLeaseHeld
			}
			return fn(ctx)
		})
		if !errors.Is(err, errFileMergeLeaseHeld) {
			return err
		}
		timer := time.NewTimer(f.cfg.MergeLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("acquire file merge lock: live plane lease held by run %s: %w", holder, ctx.Err())
		case <-timer.C:
		}
	}
}

func (f *File) livePlaneMergeLease() (string, error) {
	holder := ""
	check := func() error {
		session, err := f.open()
		if err != nil {
			return err
		}
		now := time.Now()
		for _, entry := range session.ledger.Snapshot() {
			if entry.IsPublicationClaim() || !entry.ExpiresAt.After(now) {
				continue
			}
			externalID := entry.ExternalID
			if externalID == "" {
				externalID = entry.ItemID
			}
			if strings.HasPrefix(externalID, MergeLockItemPrefix) {
				holder = entry.RunID
				return nil
			}
		}
		return nil
	}
	if f.cfg.Lock == nil {
		return holder, check()
	}
	return holder, f.cfg.Lock(fileOperationMergeCheck, check)
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

func (s *fileSession) ClaimScoped(ctx context.Context, key Key, runID, workflow string, lease time.Duration) (bool, string, error) {
	if s.file.cfg.Shared != nil && !localscheduler.IsPublicationClaimKey(key.ExternalID) {
		return false, "", ErrSharedRequiresUnlocked
	}
	return s.claimLocal(key, runID, workflow, lease)
}

func (s *fileSession) claimLocal(key Key, runID, workflow string, lease time.Duration) (bool, string, error) {
	if key.Gaggle == "" && key.Provider == "" {
		return s.ledger.Claim(key.ExternalID, runID, workflow, lease)
	}
	return s.ledger.ClaimScoped(key, runID, workflow, lease)
}

func (s *fileSession) ReleaseScoped(ctx context.Context, key Key, runID string, reason claimrelease.Reason) error {
	for _, entry := range s.ledger.ForRunAll(runID) {
		if KeyForEntry(entry) == key && !entry.SharedDeadline.IsZero() {
			return ErrSharedRequiresUnlocked
		}
	}
	if key.Gaggle == "" && key.Provider == "" {
		return s.ledger.Release(key.ExternalID, runID, reason)
	}
	return s.ledger.ReleaseScoped(key, runID, reason)
}

func (s *fileSession) ReleaseAllForRun(ctx context.Context, runID string, reason claimrelease.Reason) ([]Entry, error) {
	entries := s.ledger.ForRunAll(runID)
	released := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsPublicationClaim() {
			continue
		}
		if err := s.releaseEntry(ctx, entry, runID, reason); err != nil {
			return released, fmt.Errorf("release claim %s for run %s: %w", entry.ItemID, runID, err)
		}
		released = append(released, reportEntry(entry))
	}
	return released, nil
}

func (s *fileSession) ForRunAll(_ context.Context, runID string) ([]Entry, error) {
	entries := s.ledger.ForRunAll(runID)
	for i := range entries {
		entries[i] = reportEntry(entries[i])
	}
	return entries, nil
}

func (s *fileSession) ListNamespace(_ context.Context, gaggle, provider string) (Listing, error) {
	var listing Listing
	for _, entry := range s.ledger.Snapshot() {
		if InNamespace(entry, gaggle, provider) {
			listing.Entries = append(listing.Entries, reportEntry(entry))
		}
	}
	for _, entry := range s.ledger.HistorySnapshot() {
		if InNamespace(entry, gaggle, provider) {
			listing.History = append(listing.History, reportEntry(entry))
		}
	}
	return listing, nil
}

func (s *fileSession) MergeLock(ctx context.Context, lock MergeLock, fn func(context.Context) error) error {
	return s.file.MergeLock(ctx, lock, fn)
}

// Match the HTTP reporting boundary without rewriting legacy ledger records.
func reportEntry(entry Entry) Entry {
	entry.Verification = entry.Verification.Report()
	return entry
}

// ReleaseExpected releases a snapshot only if its exact incarnation still owns
// the item. It reports whether cleanup actually removed that incarnation.
func (f *File) ReleaseExpected(ctx context.Context, expected Entry, reason claimrelease.Reason) (bool, error) {
	return f.releaseExpected(ctx, expected, reason, time.Time{})
}

// ReleaseExpiredExpected also rechecks the current deadline under the claims
// lock and, for shared claims, the transition lock before reserving cleanup.
// A stale expiry scan must not release a successfully renewed same-owner lease.
func (f *File) ReleaseExpiredExpected(ctx context.Context, expected Entry, reason claimrelease.Reason, expiredAt time.Time) (bool, error) {
	if expiredAt.IsZero() {
		return false, fmt.Errorf("claimsclient: expiry cleanup requires a cutoff")
	}
	return f.releaseExpected(ctx, expected, reason, expiredAt)
}

func (f *File) releaseExpected(ctx context.Context, expected Entry, reason claimrelease.Reason, expiredAt time.Time) (released bool, err error) {
	if !expected.SharedDeadline.IsZero() {
		return f.releaseShared(ctx, expected, reason, expiredAt)
	}
	err = f.Locked(ctx, f.operationName(fileOperationRelease), func(tx Ledger) error {
		session := tx.(*fileSession)
		current, held := matchingEntry(session.ledger, expected)
		if !held || (!expiredAt.IsZero() && current.ExpiresAt.After(expiredAt)) {
			return nil
		}
		err := session.releaseEntry(ctx, current, current.RunID, reason)
		released = err == nil
		return err
	})
	return released, err
}

func (f *File) operationName(fallback string) string {
	if f.operation != "" {
		return f.operation
	}
	return fallback
}

// Sequence labels individual primitive operations without holding a claims
// lock around selection or provider I/O. Admission itself arbitrates races,
// just as it does for the HTTP backend. Use Locked for purely local atomic work.
func Sequence(ctx context.Context, ledger Ledger, operation string, fn func(Ledger) error) error {
	if file, ok := ledger.(*File); ok {
		scoped := *file
		scoped.operation = operation
		return fn(&scoped)
	}
	return fn(ledger)
}

func matchingEntry(ledger FileLedger, expected Entry) (Entry, bool) {
	for _, current := range ledger.ForRunAll(expected.RunID) {
		if KeyForEntry(current) == KeyForEntry(expected) && current.ClaimedAt.Equal(expected.ClaimedAt) && current.SharedOwner == expected.SharedOwner {
			return current, true
		}
	}
	return Entry{}, false
}

// RenewExpected renews only the snapshot's current incarnation. Shared renewal
// additionally requires unexpired execution authority before the remote write.
func (f *File) RenewExpected(ctx context.Context, expected Entry, ttl time.Duration) (ok bool, err error) {
	if expected.IsPublicationClaim() {
		return false, nil
	}
	if !expected.SharedDeadline.IsZero() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if f.cfg.Shared == nil || f.cfg.Lock == nil {
			return false, ErrSharedRequiresUnlocked
		}
		binding, err := f.cfg.Shared.Admission(ctx, KeyForEntry(expected), expected.RunID, expected.Workflow)
		if err != nil {
			return false, err
		}
		if binding == nil || binding.Owner != expected.SharedOwner {
			return false, fmt.Errorf("shared renewal requires its persisted owner")
		}
		ok, _, err := f.claimShared(ctx, KeyForEntry(expected), expected.RunID, expected.Workflow, ttl, *binding, &expected)
		return ok, err
	}
	err = f.Locked(ctx, f.operationName(fileOperationClaim), func(tx Ledger) error {
		session := tx.(*fileSession)
		current, held := matchingEntry(session.ledger, expected)
		if !held {
			return nil
		}
		var err error
		ok, _, err = session.claimLocal(KeyForEntry(current), current.RunID, current.Workflow, ttl)
		return err
	})
	return ok, err
}
