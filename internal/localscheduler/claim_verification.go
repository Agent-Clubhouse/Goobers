package localscheduler

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidClaimVerification distinguishes bad observations from store failures.
var ErrInvalidClaimVerification = errors.New("invalid claim verification")

// ClaimVerification is the last provider observation for this exact lease.
// A zero observation means unverified, never inferred from ledger ownership.
type ClaimVerification struct {
	State         string    `json:"state"`
	ObservedAt    time.Time `json:"observedAt"`
	ProviderRunID string    `json:"providerRunId,omitempty"`
}

// Report makes the absence of an observation explicit without inventing a
// provider check or changing legacy persisted entries.
func (v ClaimVerification) Report() ClaimVerification {
	if v.State == "" {
		v.State = "unverified"
	}
	return v
}

func validateClaimObservation(expected ClaimEntry, observation ClaimVerification) error {
	if observation.State != "verified" && observation.State != "missing" && observation.State != "ownership-mismatch" && observation.State != "unavailable" {
		return fmt.Errorf("%w: unsupported state", ErrInvalidClaimVerification)
	}
	if len(observation.ProviderRunID) > 1024 {
		return fmt.Errorf("%w: provider owner exceeds 1024 bytes", ErrInvalidClaimVerification)
	}
	if observation.ObservedAt.IsZero() || observation.ObservedAt.Before(expected.ClaimedAt) {
		return fmt.Errorf("%w: observation predates lease", ErrInvalidClaimVerification)
	}
	if (observation.State == "verified" && observation.ProviderRunID != expected.RunID) ||
		(observation.State == "ownership-mismatch" && (observation.ProviderRunID == "" || observation.ProviderRunID == expected.RunID)) ||
		((observation.State == "missing" || observation.State == "unavailable") && observation.ProviderRunID != "") {
		return fmt.Errorf("%w: state contradicts provider owner", ErrInvalidClaimVerification)
	}
	return nil
}

// RecordClaimVerification does not grant or extend a lease. Compare-and-swap
// protects a replacement lease from a slow observation of its predecessor.
func (l *ClaimLedger) RecordClaimVerification(expected ClaimEntry, observation ClaimVerification) (bool, error) {
	if err := validateClaimObservation(expected, observation); err != nil {
		return false, err
	}
	storageKey := expected.ItemID
	if expected.Gaggle != "" || expected.Provider != "" {
		var err error
		storageKey, err = (ClaimKey{Gaggle: expected.Gaggle, Provider: expected.Provider, ExternalID: expected.ExternalID}).storageKey()
		if err != nil {
			return false, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if observation.ObservedAt.After(now) {
		return false, fmt.Errorf("%w: observation is in the future", ErrInvalidClaimVerification)
	}
	current, ok := l.entries[storageKey]
	if !ok || current.RunID != expected.RunID || !current.ClaimedAt.Equal(expected.ClaimedAt) || current.expired(now) || current.ReleasedAt != nil {
		return false, nil
	}
	if !current.Verification.ObservedAt.IsZero() && !observation.ObservedAt.After(current.Verification.ObservedAt) {
		return false, nil
	}
	updated := current
	updated.Verification = observation
	l.entries[storageKey] = updated
	previousHistory, hadHistory := l.historyEntry(current.RunID, storageKey)
	l.recordHistory(storageKey, updated)
	if err := l.persist(); err != nil {
		l.entries[storageKey] = current
		l.restoreHistory(current.RunID, storageKey, previousHistory, hadHistory)
		return false, err
	}
	return true, nil
}
