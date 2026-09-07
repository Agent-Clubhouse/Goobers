package claimsclient

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/localscheduler"
)

// VerificationRecorder records observations without granting or renewing a
// lease. All production backends implement it; it is separate from ownership
// primitives so a reporting failure cannot be mistaken for lost ownership.
type VerificationRecorder interface {
	RecordClaimVerification(context.Context, Entry, localscheduler.ClaimVerification) (bool, error)
}

// Restated from httpapi.ClaimVerificationRequest; a wire-shape test pins it.
type claimVerificationRequest struct {
	RunID       string                           `json:"runId"`
	Gaggle      string                           `json:"gaggle"`
	Provider    string                           `json:"provider"`
	ItemID      string                           `json:"itemId"`
	OwnerRunID  string                           `json:"ownerRunId"`
	ClaimedAt   time.Time                        `json:"claimedAt"`
	Observation localscheduler.ClaimVerification `json:"observation"`
}

// RecordClaimVerification submits an exact-lease observation to the daemon.
func (h *HTTP) RecordClaimVerification(ctx context.Context, entry Entry, observation localscheduler.ClaimVerification) (bool, error) {
	if err := scopedKey(KeyForEntry(entry)); err != nil {
		return false, err
	}
	var response claimResponse
	err := h.post(ctx, apicontract.ClaimVerifyPath, claimVerificationRequest{
		RunID: h.cfg.RunID, Gaggle: entry.Gaggle, Provider: entry.Provider,
		ItemID: entry.ExternalID, OwnerRunID: entry.RunID, ClaimedAt: entry.ClaimedAt, Observation: observation,
	}, &response)
	return response.Ok, err
}

// RecordClaimVerification updates the ledger under its existing file lock.
func (f *File) RecordClaimVerification(ctx context.Context, entry Entry, observation localscheduler.ClaimVerification) (ok bool, err error) {
	err = f.Locked(ctx, "claim.verification", func(session Ledger) error {
		var recordErr error
		ok, recordErr = session.(VerificationRecorder).RecordClaimVerification(ctx, entry, observation)
		return recordErr
	})
	return ok, err
}

func (s *fileSession) RecordClaimVerification(_ context.Context, entry Entry, observation localscheduler.ClaimVerification) (bool, error) {
	return s.ledger.RecordClaimVerification(entry, observation)
}

var (
	_ VerificationRecorder = (*File)(nil)
	_ VerificationRecorder = (*fileSession)(nil)
	_ VerificationRecorder = (*HTTP)(nil)
)
