package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

// Confirmation for shared runs never participates in the legacy comment
// election. Labels mirror the provider lease, and a label failure cannot revoke
// it. The second authority read fences a successor or expiry during label IO.
func confirmSharedClaimVisibility(ctx context.Context, entry claimsclient.Entry, resolver claimsclient.SharedClaimResolver, labels sharedclaim.Visibility, stderr io.Writer) (providers.ClaimResult, error) {
	if resolver == nil {
		return providers.ClaimResult{}, fmt.Errorf("shared claim confirmation requires its resolver")
	}
	binding, err := resolver.Admission(ctx, claimsclient.KeyForEntry(entry), entry.RunID, entry.Workflow)
	if err != nil {
		return providers.ClaimResult{}, err
	}
	if binding == nil {
		return providers.ClaimResult{}, fmt.Errorf("shared claim confirmation requires a shared run pin")
	}
	if err := verifySharedVisibilityOwner(ctx, entry, *binding); err != nil {
		return providers.ClaimResult{}, err
	}
	labelCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = sharedclaim.ReconcileVisibility(labelCtx, binding.Store, labels, binding.RemoteKey)
	cancel()
	if err != nil {
		pf(stderr, "warning: shared claim label for item %s needs reconciliation: %v\n", entry.ExternalID, err)
	}
	if err := verifySharedVisibilityOwner(ctx, entry, *binding); err != nil {
		return providers.ClaimResult{}, err
	}
	return providers.ClaimResult{Claimed: true, ClaimedBy: entry.RunID}, nil
}

func verifySharedVisibilityOwner(ctx context.Context, entry claimsclient.Entry, binding claimsclient.SharedClaimBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if binding.Store == nil || binding.Owner != entry.SharedOwner || entry.SharedOwner == (sharedclaim.Owner{}) || entry.SharedRevoked ||
		!time.Now().Before(entry.SharedDeadline) || !time.Now().Before(entry.ExpiresAt) {
		return fmt.Errorf("shared claim is no longer admitted locally")
	}
	observed, err := binding.Store.Read(ctx, binding.RemoteKey)
	if err != nil {
		return err
	}
	if _, err := sharedclaim.Encode(binding.RemoteKey, observed.Record); err != nil {
		return err
	}
	if observed.Revision == "" || observed.Now.IsZero() || observed.Record.Owner != entry.SharedOwner || !observed.Now.Before(observed.Record.ExpiresAt) {
		return fmt.Errorf("shared claim is no longer owned remotely")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(entry.SharedDeadline) || !time.Now().Before(entry.ExpiresAt) {
		return fmt.Errorf("shared claim expired during confirmation")
	}
	return nil
}
