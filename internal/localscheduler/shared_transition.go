package localscheduler

import (
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

func (e ClaimEntry) transitionPending() bool { return e.ReleasePending || e.SharedAdmissionPending }

// BeginSharedAdmission reserves local custody before remote admission. Callers
// hold the per-item transition lock across reserve, provider I/O and finalize,
// and the claims lock only for each ledger operation. An uncertain admission
// survives restart as an expired reservation, never execution authority.
func (l *ClaimLedger) BeginSharedAdmission(key ClaimKey, owner sharedclaim.Owner, workflow string) (ClaimEntry, bool, string, error) {
	if IsPublicationClaimKey(key.ExternalID) {
		return ClaimEntry{}, false, "", errors.New("publication custody cannot use shared admission")
	}
	storageKey, err := key.storageKey()
	if err != nil {
		return ClaimEntry{}, false, "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, err := sharedclaim.Encode(key.ExternalID, sharedclaim.Record{Version: 1, Owner: owner, ExpiresAt: now}); err != nil {
		return ClaimEntry{}, false, "", err
	}
	if historical, ok := l.historyEntry(owner.Run, storageKey); ok && historical.SharedRevoked {
		return ClaimEntry{}, false, "", errors.New("shared execution was administratively revoked")
	}
	if legacy, held := l.entries[key.ExternalID]; held && (legacy.transitionPending() || !legacy.expired(now)) {
		return ClaimEntry{}, false, legacy.RunID, nil
	}
	current, held := l.entries[storageKey]
	if held && (current.ReleasePending || current.SharedRevoked ||
		(current.SharedOwner != (sharedclaim.Owner{}) && current.SharedOwner != owner) ||
		(!current.expired(now) && current.RunID != owner.Run)) {
		return ClaimEntry{}, false, current.RunID, nil
	}
	if current.SharedAdmissionPending {
		return current, true, current.RunID, nil
	}
	reserved := current
	if !held || current.SharedOwner == (sharedclaim.Owner{}) {
		// A local-only lease must not survive conversion to remote admission.
		reserved = ClaimEntry{ItemID: key.ExternalID, ExternalID: key.ExternalID, Gaggle: key.Gaggle, Provider: key.Provider,
			RunID: owner.Run, Workflow: workflow, ClaimedAt: now, ExpiresAt: now, SharedDeadline: now, SharedOwner: owner}
	}
	reserved.SharedAdmissionPending = true
	previousHistory, hadHistory := l.historyEntry(owner.Run, storageKey)
	l.entries[storageKey] = reserved
	l.recordHistory(storageKey, reserved)
	if err := l.persist(); err != nil {
		if held {
			l.entries[storageKey] = current
		} else {
			delete(l.entries, storageKey)
		}
		l.restoreHistory(owner.Run, storageKey, previousHistory, hadHistory)
		return ClaimEntry{}, false, "", err
	}
	return reserved, true, owner.Run, nil
}

// FinishSharedAdmission grants execution only after remote acknowledgment and
// revalidation of the exact reserved incarnation. Revocation wins even when a
// remote write completes after an administrator has stopped local execution.
func (l *ClaimLedger) FinishSharedAdmission(expected ClaimEntry, deadline time.Time) (bool, string, error) {
	storageKey, err := claimEntryStorageKey(expected)
	if err != nil {
		return false, "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, held := l.entries[storageKey]
	if !held || current.RunID != expected.RunID || current.SharedOwner != expected.SharedOwner || !current.ClaimedAt.Equal(expected.ClaimedAt) || !current.SharedAdmissionPending || current.ReleasePending {
		return false, current.RunID, nil
	}
	if current.SharedRevoked {
		return false, "", errors.New("shared execution was administratively revoked")
	}
	if !deadline.After(l.now()) {
		return false, "", fmt.Errorf("shared admission expired before finalization")
	}
	admitted := current
	admitted.SharedAdmissionPending = false
	admitted.SharedDeadline, admitted.ExpiresAt = deadline, deadline
	if err := l.storeClaim(storageKey, admitted); err != nil {
		return false, "", err
	}
	return true, admitted.RunID, nil
}

// BeginSharedRelease reserves cleanup of a persisted shared owner, including
// an admission whose acknowledgment was lost. The transition lock serializes
// release against renewal; expired execution does not discard cleanup custody.
// A nonzero expiredAt additionally requires the current lease to be expired at
// that cutoff, checked atomically with reservation under the claims lock.
func (l *ClaimLedger) BeginSharedRelease(expected ClaimEntry, expiredAt time.Time) (bool, error) {
	if expected.SharedOwner == (sharedclaim.Owner{}) || expected.SharedDeadline.IsZero() {
		return false, errors.New("shared release requires persisted ownership")
	}
	return l.beginProviderRelease(expected, true, expiredAt)
}
