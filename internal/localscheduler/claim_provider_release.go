package localscheduler

import (
	"errors"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
)

// ErrClaimReleasePending prevents ordinary release from discarding cleanup custody.
var ErrClaimReleasePending = errors.New("claim provider release is pending; retry provider cleanup")

func claimEntryStorageKey(entry ClaimEntry) (string, error) {
	if entry.Gaggle == "" && entry.Provider == "" {
		return entry.ItemID, nil
	}
	return (ClaimKey{Gaggle: entry.Gaggle, Provider: entry.Provider, ExternalID: entry.ExternalID}).storageKey()
}

// BeginProviderRelease compares the snapshot's incarnation and durably reserves
// cleanup custody. The caller must serialize cleanup attempts with a separate
// per-item process lock, held until provider I/O and FinishProviderRelease end.
// A crashed attempt leaves custody in claims.json; the next lock holder retries
// cleanup before admitting another owner, even if the execution lease expired.
func (l *ClaimLedger) BeginProviderRelease(expected ClaimEntry) (bool, error) {
	return l.beginProviderRelease(expected, false)
}

func (l *ClaimLedger) beginProviderRelease(expected ClaimEntry, shared bool) (bool, error) {
	if expected.IsPublicationClaim() {
		return false, errors.New("publication custody cannot use provider cleanup")
	}
	key, err := claimEntryStorageKey(expected)
	if err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, held := l.entries[key]
	if !held || current.RunID != expected.RunID || !current.ClaimedAt.Equal(expected.ClaimedAt) || current.SharedOwner != expected.SharedOwner {
		return false, nil
	}
	if !current.SharedDeadline.IsZero() != shared {
		return false, errors.New("shared claims require coordinated release")
	}
	if current.ReleasePending {
		return true, nil
	}
	updated := current
	updated.ReleasePending = true
	updated.SharedAdmissionPending = false
	previous, hadHistory := l.historyEntry(current.RunID, key)
	l.entries[key] = updated
	l.recordHistory(key, updated)
	if err := l.persist(); err != nil {
		l.entries[key] = current
		l.restoreHistory(current.RunID, key, previous, hadHistory)
		return false, err
	}
	return true, nil
}

// FinishProviderRelease atomically removes only the reserved incarnation after
// acknowledged provider cleanup. Failed provider calls leave the reservation
// intact and retryable. Renewals and ordinary releases cannot bypass custody.
func (l *ClaimLedger) FinishProviderRelease(expected ClaimEntry, reason claimrelease.Reason) (bool, error) {
	key, err := claimEntryStorageKey(expected)
	if err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, held := l.entries[key]
	if !held || current.RunID != expected.RunID || !current.ClaimedAt.Equal(expected.ClaimedAt) || !current.ReleasePending {
		return false, nil
	}
	if current.IsPublicationClaim() {
		return false, errors.New("publication custody cannot use provider cleanup")
	}
	previous, hadHistory := l.historyEntry(current.RunID, key)
	released := current
	released.ReleasePending = false
	reason = reason.Normalize()
	l.recordReleasedHistory(key, released, l.now(), reason)
	delete(l.entries, key)
	if err := l.persist(); err != nil {
		l.entries[key] = current
		l.restoreHistory(current.RunID, key, previous, hadHistory)
		return false, err
	}
	l.journalRelease(journal.EventClaimReleased, released, reason, nil)
	return true, nil
}
