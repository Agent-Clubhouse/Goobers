package localscheduler

import (
	"fmt"

	"github.com/goobers/goobers/internal/sharedclaim"
)

// Validate before exposing a reopened ledger to any cleanup, renewal, or
// reclaim path. Partial shared markers must not become local-only leases.
// History is checked before pruning because it can authorize run recovery.
func (l *ClaimLedger) validateSharedClaims() error {
	for key, entry := range l.entries {
		if err := validateSharedClaimEntry(key, entry); err != nil {
			return err
		}
	}
	for runID, entries := range l.history {
		for key, entry := range entries {
			if err := validateSharedClaimEntry(key, entry); err != nil {
				return err
			}
			if entry.SharedOwner != (sharedclaim.Owner{}) && entry.RunID != runID {
				return fmt.Errorf("shared claim history belongs to another run")
			}
		}
	}
	return nil
}

func validateSharedClaimEntry(storageKey string, entry ClaimEntry) error {
	if entry.SharedOwner == (sharedclaim.Owner{}) && entry.SharedDeadline.IsZero() {
		return nil // legacy and explicitly local records remain compatible
	}
	if entry.SharedOwner == (sharedclaim.Owner{}) || entry.SharedDeadline.IsZero() || entry.SharedOwner.Run != entry.RunID {
		return fmt.Errorf("shared claim requires matching owner and deadline")
	}
	key := ClaimKey{Gaggle: entry.Gaggle, Provider: entry.Provider, ExternalID: entry.ExternalID}
	expected, err := key.storageKey()
	if err != nil || expected != storageKey || entry.ItemID != entry.ExternalID {
		return fmt.Errorf("shared claim does not match its ledger key")
	}
	if !entry.ExpiresAt.Equal(entry.SharedDeadline) {
		return fmt.Errorf("shared claim expiry differs from its admission deadline")
	}
	_, err = sharedclaim.Encode(entry.ExternalID, sharedclaim.Record{Version: 1, Owner: entry.SharedOwner, ExpiresAt: entry.SharedDeadline})
	return err
}
