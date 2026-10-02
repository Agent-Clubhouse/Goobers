package providers

import (
	"context"
	"fmt"
)

type claimRESTWorkItemHooks struct {
	ready                   func() error
	restoreOwnedClaimLabel  func(context.Context, RepositoryRef, string, string) error
	missingBreadcrumbWinner func(string) string
	waitForClaimLabel       func(context.Context, RepositoryRef, string, string, string) (WorkItem, error)
}

func claimRESTWorkItem(
	ctx context.Context,
	c restClaimMutationProvider,
	kind ProviderKind,
	baseURL string,
	attribution Attribution,
	req ClaimWorkItemRequest,
	hooks claimRESTWorkItemHooks,
) (ClaimResult, error) {
	if hooks.ready != nil {
		if err := hooks.ready(); err != nil {
			return ClaimResult{}, err
		}
	}
	if err := requireOwnerRepo(req.Repository); err != nil {
		return ClaimResult{}, err
	}
	if req.ID == "" {
		return ClaimResult{}, errIssueIDRequired
	}
	if req.RunID == "" {
		return ClaimResult{}, fmt.Errorf("run id is required to claim an item")
	}
	label := req.ClaimLabel
	if label == "" {
		label = LabelClaimed
	}

	if winner, ok, err := claimWinner(ctx, c, baseURL, req.Repository, req.ID); err != nil {
		return ClaimResult{}, err
	} else if ok {
		if winner == req.RunID && hooks.restoreOwnedClaimLabel != nil {
			if err := hooks.restoreOwnedClaimLabel(ctx, req.Repository, req.ID, label); err != nil {
				return ClaimResult{}, err
			}
		}
		return finishRESTWorkItemClaim(ctx, c, kind, req, winner, label, hooks)
	}

	if err := postAttributedComment(ctx, c, baseURL, attribution, req.Repository, req.ID, claimBreadcrumb(req.RunID), "claim"); err != nil {
		return ClaimResult{}, err
	}
	winner, ok, err := claimWinner(ctx, c, baseURL, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		if hooks.missingBreadcrumbWinner == nil {
			return ClaimResult{}, fmt.Errorf("claim breadcrumb for run %q is not visible after write", req.RunID)
		}
		winner = hooks.missingBreadcrumbWinner(req.RunID)
	}
	if winner == req.RunID {
		if err := c.applyLabelChanges(ctx, req.Repository, req.ID, []string{label}, nil); err != nil {
			return ClaimResult{}, err
		}
	}
	return finishRESTWorkItemClaim(ctx, c, kind, req, winner, label, hooks)
}

func finishRESTWorkItemClaim(
	ctx context.Context,
	c restClaimMutationProvider,
	kind ProviderKind,
	req ClaimWorkItemRequest,
	winner string,
	label string,
	hooks claimRESTWorkItemHooks,
) (ClaimResult, error) {
	item, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return ClaimResult{}, err
	}
	claimed := winner == req.RunID
	if claimed && !item.HasLabel(label) && hooks.waitForClaimLabel != nil {
		item, err = hooks.waitForClaimLabel(ctx, req.Repository, req.ID, req.RunID, label)
		if err != nil {
			return ClaimResult{}, err
		}
	}
	providerRunID := ""
	if !claimed {
		providerRunID = winner
	}
	c.recordExternalRef(ctx, ExternalRef{
		Provider: kind, Ref: issueRef(req.Repository, req.ID), URL: item.URL,
		Operation: "claim", Outcome: claimAttemptOutcome(claimed),
		RunID: req.RunID, ProviderRunID: providerRunID,
		Fields: map[string]FieldDigest{
			"claim": {After: digestString("run=" + winner)},
		},
	})
	return ClaimResult{Claimed: claimed, ClaimedBy: winner, Item: item}, nil
}
