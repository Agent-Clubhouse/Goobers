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
	// The pre-IO snapshot is unlocked, so the lease can lapse or change hands
	// while the marker is written. A provider marker must only mirror a ledger
	// grant: reconfirm the lease and retract the marker if it is not this one.
	held, err := session.reconfirmLease(ctx, owned)
	if err != nil || held {
		return result, err
	}
	if _, err := session.env.issueProvider.ReleaseWorkItemClaim(ctx, request); err != nil {
		// The open provider epoch would block the lease's new owner, so the
		// stage must fail rather than carry on as though cleanup succeeded.
		return providers.ClaimResult{}, fmt.Errorf("retract provider claim on item %s after its ledger lease was lost: %w", item.ID, err)
	}
	return providers.ClaimResult{}, &ledgerLeaseLostError{itemID: item.ID}
}

// ledgerLeaseLostError reports that this run's ledger lease ended or passed to
// another run while its provider claim marker was being written.
type ledgerLeaseLostError struct{ itemID string }

func (e *ledgerLeaseLostError) Error() string {
	return fmt.Sprintf("ledger lease for item %s was lost while its provider claim was written", e.itemID)
}

// reconfirmLease reports whether owned is still this run's live, unreleased
// lease incarnation. It reads under the claims lock so no renewal or
// recovery sweep can move the lease between the read and the decision.
// ClaimedAt identifies the incarnation: renewals keep it, and a replacement
// or a re-grant after expiry never does. Liveness uses the session clock,
// which on a self runner is the clock the local ledger itself judges expiry
// by, so a provider write that outlasts the lease cannot keep its marker.
func (session *backlogClaimSession) reconfirmLease(ctx context.Context, owned claimsclient.Entry) (bool, error) {
	key := claimsclient.KeyForEntry(owned)
	held := false
	err := session.ledger.Locked(ctx, claimLockOperationBacklogClaim, func(tx claimsclient.Ledger) error {
		entries, err := tx.ForRunAll(ctx, session.runID)
		if err != nil {
			return err
		}
		now := session.clock()
		for _, entry := range entries {
			if claimsclient.KeyForEntry(entry) == key {
				held = entry.ClaimedAt.Equal(owned.ClaimedAt) && entry.ReleasedAt == nil && entry.ExpiresAt.After(now)
			}
		}
		return nil
	})
	return held, err
}

func (session *backlogClaimSession) clock() time.Time {
	if session.now != nil {
		return session.now()
	}
	return time.Now()
}
