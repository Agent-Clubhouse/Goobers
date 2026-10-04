package providers

import (
	"context"
	"fmt"
)

type claimProtocolHooks struct {
	validate                func() (string, error)
	winner                  func(context.Context) (string, bool, error)
	postClaim               func(context.Context) error
	restoreOwnedLabel       func(context.Context, string) error
	addLabel                func(context.Context, string) error
	finish                  func(context.Context, string, string) (ClaimResult, error)
	missingBreadcrumbWinner func(string) string
}

func claimWithProtocol(ctx context.Context, runID string, hooks claimProtocolHooks) (ClaimResult, error) {
	label, err := hooks.validate()
	if err != nil {
		return ClaimResult{}, err
	}
	if winner, ok, err := hooks.winner(ctx); err != nil {
		return ClaimResult{}, err
	} else if ok {
		if winner == runID && hooks.restoreOwnedLabel != nil {
			if err := hooks.restoreOwnedLabel(ctx, label); err != nil {
				return ClaimResult{}, err
			}
		}
		return hooks.finish(ctx, winner, label)
	}
	if err := hooks.postClaim(ctx); err != nil {
		return ClaimResult{}, err
	}
	winner, ok, err := hooks.winner(ctx)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		if hooks.missingBreadcrumbWinner == nil {
			return ClaimResult{}, fmt.Errorf("claim breadcrumb for run %q is not visible after write", runID)
		}
		winner = hooks.missingBreadcrumbWinner(runID)
	}
	if winner == runID {
		if err := hooks.addLabel(ctx, label); err != nil {
			return ClaimResult{}, err
		}
	}
	return hooks.finish(ctx, winner, label)
}

type releaseClaimProtocolHooks struct {
	validate                 func() (string, error)
	winner                   func(context.Context) (string, bool, error)
	getItem                  func(context.Context) (WorkItem, error)
	postRelease              func(context.Context, string) error
	hasLabel                 func(WorkItem, string) bool
	removeLabel              func(context.Context, string) (WorkItem, error)
	restoreLabel             func(context.Context, string) (WorkItem, error)
	finishWithoutLabel       func(context.Context, WorkItem) (WorkItem, error)
	readItemBeforeBreadcrumb bool
}

func releaseClaimWithProtocol(ctx context.Context, req ClaimWorkItemRequest, hooks releaseClaimProtocolHooks) (WorkItem, WorkItem, string, error) {
	label, err := hooks.validate()
	if err != nil {
		return WorkItem{}, WorkItem{}, "", err
	}
	winner, claimed, err := hooks.winner(ctx)
	if err != nil {
		return WorkItem{}, WorkItem{}, "", err
	}
	if claimed && req.ExpectedClaimRunID != "" && winner != req.ExpectedClaimRunID {
		return WorkItem{}, WorkItem{}, "", fmt.Errorf("provider claim is held by run %q, not expected run %q", winner, req.ExpectedClaimRunID)
	}
	if claimed && winner != req.RunID && !req.LedgerAuthorized {
		return WorkItem{}, WorkItem{}, "", fmt.Errorf("provider claim is held by run %q", winner)
	}

	var before WorkItem
	if hooks.readItemBeforeBreadcrumb {
		before, err = hooks.getItem(ctx)
		if err != nil {
			return WorkItem{}, WorkItem{}, "", err
		}
	}
	releasedRunID := req.RunID
	if claimed {
		releasedRunID = winner
		if err := hooks.postRelease(ctx, winner); err != nil {
			return WorkItem{}, WorkItem{}, "", err
		}
	}
	if !hooks.readItemBeforeBreadcrumb {
		before, err = hooks.getItem(ctx)
		if err != nil {
			return WorkItem{}, WorkItem{}, "", err
		}
	}
	if req.ExpectedClaimRunID != "" {
		if current, currentClaimed, err := hooks.winner(ctx); err != nil {
			return WorkItem{}, WorkItem{}, "", err
		} else if currentClaimed {
			final, err := hooks.getItem(ctx)
			if err != nil {
				return WorkItem{}, WorkItem{}, "", err
			}
			return before, final, releasedRunID, nil
		} else if current != "" {
			return WorkItem{}, WorkItem{}, "", fmt.Errorf("provider claim owner changed to run %q after release", current)
		}
	}
	if !hooks.hasLabel(before, label) {
		if hooks.finishWithoutLabel != nil {
			final, err := hooks.finishWithoutLabel(ctx, before)
			if err != nil {
				return WorkItem{}, WorkItem{}, "", err
			}
			return before, final, releasedRunID, nil
		}
		return before, before, releasedRunID, nil
	}
	final, err := hooks.removeLabel(ctx, label)
	if err != nil {
		return WorkItem{}, WorkItem{}, "", err
	}
	if req.ExpectedClaimRunID != "" && hooks.restoreLabel != nil {
		if _, currentClaimed, err := hooks.winner(ctx); err != nil {
			return WorkItem{}, WorkItem{}, "", err
		} else if currentClaimed {
			final, err = hooks.restoreLabel(ctx, label)
			if err != nil {
				return WorkItem{}, WorkItem{}, "", err
			}
		}
	}
	return before, final, releasedRunID, nil
}

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
	return claimWithProtocol(ctx, req.RunID, claimProtocolHooks{
		validate: func() (string, error) {
			if hooks.ready != nil {
				if err := hooks.ready(); err != nil {
					return "", err
				}
			}
			if err := requireOwnerRepo(req.Repository); err != nil {
				return "", err
			}
			if req.ID == "" {
				return "", errIssueIDRequired
			}
			if req.RunID == "" {
				return "", fmt.Errorf("run id is required to claim an item")
			}
			if req.ClaimLabel == "" {
				return LabelClaimed, nil
			}
			return req.ClaimLabel, nil
		},
		winner: func(ctx context.Context) (string, bool, error) {
			return claimWinner(ctx, c, baseURL, req.Repository, req.ID)
		},
		postClaim: func(ctx context.Context) error {
			return postAttributedComment(ctx, c, baseURL, attribution, req.Repository, req.ID, claimBreadcrumb(req.RunID), "claim")
		},
		restoreOwnedLabel: func(ctx context.Context, label string) error {
			if hooks.restoreOwnedClaimLabel == nil {
				return nil
			}
			return hooks.restoreOwnedClaimLabel(ctx, req.Repository, req.ID, label)
		},
		addLabel: func(ctx context.Context, label string) error {
			return c.applyLabelChanges(ctx, req.Repository, req.ID, []string{label}, nil)
		},
		finish: func(ctx context.Context, winner, label string) (ClaimResult, error) {
			return finishRESTWorkItemClaim(ctx, c, kind, req, winner, label, hooks)
		},
		missingBreadcrumbWinner: hooks.missingBreadcrumbWinner,
	})
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
