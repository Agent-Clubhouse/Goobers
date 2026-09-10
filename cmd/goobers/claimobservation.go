package main

import (
	"context"
	"io"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

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

// confirmProviderClaim snapshots the lease before provider IO so a delayed
// response cannot annotate a replacement lease. Reporting is best-effort and
// does not change the existing claim arbitration or rollback result.
func (session *backlogClaimSession) confirmProviderClaim(ctx context.Context, item providers.WorkItem) (providers.ClaimResult, error) {
	entries, listErr := session.ledger.ForRunAll(ctx, session.runID)
	if listErr != nil {
		// Without the ledger snapshot we cannot distinguish a shared lease
		// from a legacy mirror. Never fall through to comment arbitration.
		return providers.ClaimResult{}, listErr
	}
	key := session.claimKey(item)
	for _, entry := range entries {
		if claimsclient.KeyForEntry(entry) == key && !entry.SharedDeadline.IsZero() {
			labels := providers.GitHubSharedClaimVisibility{Provider: session.env.ghIssueProvider, Repository: session.env.backlogRepo}
			result, err := confirmSharedClaimVisibility(ctx, entry, stageSharedClaimResolver(session.env.layout), labels, session.env.stderr)
			recordProviderClaimObservation(ctx, session.ledger, entry, session.env.backlogRepo, result, err, session.env.stderr)
			return result, err
		}
	}
	result, err := session.env.issueProvider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{
		Repository: session.env.backlogRepo, ID: item.ID, RunID: session.runID,
	})
	for _, entry := range entries {
		if claimsclient.KeyForEntry(entry) == key {
			recordProviderClaimObservation(ctx, session.ledger, entry, session.env.backlogRepo, result, err, session.env.stderr)
			break
		}
	}
	return result, err
}
