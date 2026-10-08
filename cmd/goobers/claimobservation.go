package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type providerClaimOwnershipError struct {
	provider      string
	itemID        string
	claimRunID    string
	providerRunID string
}

func (e *providerClaimOwnershipError) Error() string {
	return fmt.Sprintf(
		"provider claim ownership mismatch for %s item %s: ledger owner %s, provider owner %s",
		e.provider, e.itemID, e.claimRunID, e.providerRunID,
	)
}

func (e *providerClaimOwnershipError) Code() string {
	return "provider_ledger_ownership_mismatch"
}

func recordClaimObservation(ctx context.Context, ledger claimsclient.Ledger, entry claimsclient.Entry, observation localscheduler.ClaimVerification, stderr io.Writer) {
	recorder, ok := ledger.(claimsclient.VerificationRecorder)
	if !ok {
		pf(stderr, "warning: claim verification recording is unavailable for item %s\n", entry.ItemID)
		return
	}
	if _, err := recorder.RecordClaimVerification(ctx, entry, observation); err != nil {
		pf(stderr, "warning: could not record claim verification for item %s: %v\n", entry.ItemID, err)
	}
}

func recordProviderClaimObservation(ctx context.Context, ledger claimsclient.Ledger, entry claimsclient.Entry, repo providers.RepositoryRef, result providers.ClaimResult, claimErr error, stderr io.Writer) {
	observation := localscheduler.ClaimVerification{State: "unavailable", ObservedAt: time.Now()}
	if claimErr == nil && result.Claimed {
		observation.State, observation.ProviderRunID = "verified", entry.RunID
	} else if claimErr == nil && result.ClaimedBy != "" && result.ClaimedBy != entry.RunID {
		observation.State, observation.ProviderRunID = "ownership-mismatch", result.ClaimedBy
		// This is a separate ledger/provider consistency error, not a second
		// provider-attempt event. The stage's parent owns the journal writer.
		ref := repo.Owner + "/" + repo.Name + "#" + entry.ExternalID
		if repo.Provider == providers.ProviderADO {
			ref = "ado#" + entry.ExternalID
		}
		sidecarMutationRecorder{kind: itemKindIssue}.RecordExternalRef(ctx, providers.ExternalRef{
			Provider: repo.Provider, Ref: ref, Operation: "claim-verification",
			RunID: entry.RunID, ProviderRunID: result.ClaimedBy,
			Outcome: "conflict", ErrorCode: "provider_ledger_ownership_mismatch",
		})
	}
	recordClaimObservation(ctx, ledger, entry, observation, stderr)
}

func recordProviderClaimContention(ctx context.Context, ledger claimsclient.Ledger, entry claimsclient.Entry, providerRunID string, stderr io.Writer) {
	recordClaimObservation(ctx, ledger, entry, localscheduler.ClaimVerification{
		State: "contended", ObservedAt: time.Now(), ProviderRunID: providerRunID,
	}, stderr)
}

// confirmProviderClaim snapshots the lease before provider IO so the caller
// can bind its final, post-reconciliation observation to the same lease.
func (session *backlogClaimSession) confirmProviderClaim(ctx context.Context, item providers.WorkItem) (providers.ClaimResult, error) {
	entries, listErr := session.ledger.ForRunAll(ctx, session.runID)
	if listErr != nil {
		// Without the ledger snapshot we cannot distinguish a shared lease
		// from a legacy mirror. Never fall through to comment arbitration.
		return providers.ClaimResult{}, listErr
	}
	key := session.claimKey(item)
	var owned claimsclient.Entry
	found := false
	for _, entry := range entries {
		if claimsclient.KeyForEntry(entry) != key {
			continue
		}
		found, owned = true, entry
		if !entry.SharedDeadline.IsZero() {
			labels := providers.GitHubSharedClaimVisibility{Provider: session.env.ghIssueProvider, Repository: session.env.backlogRepo}
			result, err := confirmSharedClaimVisibility(ctx, entry, stageSharedClaimResolver(session.env.layout), labels, session.env.stderr)
			return result, err
		}
	}
	if !found {
		return providers.ClaimResult{}, fmt.Errorf("claim confirmation requires an owned lease for item %s", item.ID)
	}
	request := providers.ClaimWorkItemRequest{Repository: session.env.backlogRepo, ID: item.ID, RunID: session.runID}
	result, err := session.env.issueProvider.ClaimWorkItem(ctx, request)
	if err != nil || !result.Claimed {
		return result, err
	}
	// The pre-IO snapshot is unlocked, so the lease can change hands while the
	// marker is written. A provider marker must only mirror a ledger grant:
	// re-read the lease and retract the marker if it is no longer this one.
	held, err := session.leaseStillHeld(ctx, owned)
	if err != nil || held {
		return result, err
	}
	if _, releaseErr := session.env.issueProvider.ReleaseWorkItemClaim(ctx, request); releaseErr != nil {
		pf(session.env.stderr, "warning: could not retract the provider claim on item %s after its ledger lease was lost: %v\n", item.ID, releaseErr)
	}
	return providers.ClaimResult{}, &ledgerLeaseLostError{itemID: item.ID}
}

// ledgerLeaseLostError reports that this run's ledger lease ended or passed to
// another run while its provider claim marker was being written.
type ledgerLeaseLostError struct{ itemID string }

func (e *ledgerLeaseLostError) Error() string {
	return fmt.Sprintf("ledger lease for item %s was lost while its provider claim was written", e.itemID)
}

// leaseStillHeld reports whether owned is still this run's unreleased lease
// incarnation. ClaimedAt identifies the incarnation: renewals keep it, and a
// replacement or re-grant after expiry never does.
func (session *backlogClaimSession) leaseStillHeld(ctx context.Context, owned claimsclient.Entry) (bool, error) {
	entries, err := session.ledger.ForRunAll(ctx, session.runID)
	if err != nil {
		return false, err
	}
	key := claimsclient.KeyForEntry(owned)
	for _, entry := range entries {
		if claimsclient.KeyForEntry(entry) == key && entry.ClaimedAt.Equal(owned.ClaimedAt) && entry.ReleasedAt == nil {
			return true, nil
		}
	}
	return false, nil
}
