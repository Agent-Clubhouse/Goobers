package claimsclient

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/claimrelease"
)

// Operation names identify the two short claims-lock sections in diagnostics.
const (
	ProviderReleaseReserveOperation  = "backlog-query.release.reserve"
	ProviderReleaseFinalizeOperation = "backlog-query.release.finalize"
)

// ProviderReleaser retains durable cleanup custody across provider I/O. The
// file backend invokes cleanup outside the claims lock; the HTTP backend asks
// the daemon to perform the whole operation using its trusted provider binding.
type ProviderReleaser interface {
	ReleaseWithProvider(context.Context, Entry, claimrelease.Reason, func(context.Context) error) (bool, error)
}

func (h *HTTP) ReleaseWithProvider(ctx context.Context, entry Entry, reason claimrelease.Reason, _ func(context.Context) error) (bool, error) {
	key := KeyForEntry(entry)
	if err := scopedKey(key); err != nil {
		return false, err
	}
	var response claimResponse
	// Do not use postRelease's old-daemon fallback: dropping the cleanup
	// contract would discard ownership before provider cleanup was acknowledged.
	err := h.post(ctx, apicontract.ClaimReleasePath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID,
		RunID: entry.RunID, ClaimedAt: entry.ClaimedAt,
		ProviderCleanup: true, Reason: reason.Normalize(),
	}, &response, false)
	return response.Ok, err
}

type providerReleaseLedger interface {
	BeginProviderRelease(Entry) (bool, error)
	FinishProviderRelease(Entry, claimrelease.Reason) (bool, error)
}

func (f *File) ReleaseWithProvider(ctx context.Context, entry Entry, reason claimrelease.Reason, cleanup func(context.Context) error) (released bool, err error) {
	if f.cfg.ProviderReleaseLock == nil || f.cfg.Lock == nil {
		return false, fmt.Errorf("claimsclient: provider release requires separate cleanup and claims locks")
	}
	err = f.cfg.ProviderReleaseLock(KeyForEntry(entry), func() error {
		var reserved bool
		if err := f.Locked(ctx, ProviderReleaseReserveOperation, func(tx Ledger) error {
			ledger, ok := tx.(*fileSession).ledger.(providerReleaseLedger)
			if !ok {
				return fmt.Errorf("claimsclient: provider release reservation is unavailable")
			}
			var err error
			reserved, err = ledger.BeginProviderRelease(entry)
			return err
		}); err != nil || !reserved {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := cleanup(ctx); err != nil {
			return err
		}
		return f.Locked(ctx, ProviderReleaseFinalizeOperation, func(tx Ledger) error {
			var err error
			released, err = tx.(*fileSession).ledger.(providerReleaseLedger).FinishProviderRelease(entry, reason)
			return err
		})
	})
	return released, err
}
