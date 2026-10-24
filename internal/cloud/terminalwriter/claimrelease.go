package terminalwriter

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/claims"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
)

// ReleaseClaim adapts the provider writer to the claims plane's cleanup custody.
// The ledger supplies the exact held claim; its repository comes only from the
// run's trusted pin and generation, never from the release request.
func (w *Writer) ReleaseClaim(ctx context.Context, entry localscheduler.ClaimEntry, reason claimrelease.Reason) error {
	bound, err := w.runs.Bind(ctx, entry.RunID)
	if err == nil && (bound.RunID != entry.RunID || bound.Gaggle != entry.Gaggle || string(bound.WorkItems.Provider) != entry.Provider) {
		err = providerwriter.ErrRepositoryNotPinned
	}
	if err == nil {
		err = w.ReleaseClaimMarker(ctx, providerwriter.ItemRef{RunID: entry.RunID, Repo: bound.WorkItems, ItemID: entry.ItemID}, reason)
	}
	// Scrub binding errors too; permanent refusals retain custody and become a
	// 409, while transient reads/provider failures stay retryable 503s.
	err = w.fail("release claim", err)
	if providerwriter.Permanent(err) {
		return errors.Join(claims.ErrProviderCleanupRefused, err)
	}
	return err
}
