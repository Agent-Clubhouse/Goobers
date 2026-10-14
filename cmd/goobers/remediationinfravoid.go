package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
)

// voidRemediationChargeForRun settles a pr-remediation cycle's provisional
// charge when the run that recorded it ends on an infrastructure fault
// (#5588/#5598). remediation-checkpoint charges the cause budget and records
// the pre-agent diff digest before the agent runs; when the run then dies on
// infrastructure (the harness never got an agent turn, context could not be
// materialized, ...) nothing about the fix was evaluated, but the next
// checkpoint could only read the cycle as a spent attempt and an unchanged
// diff, and escalated the PR budget-exhausted or did-not-converge.
//
// For every pull request the run holds a claim on, this marks the latest
// remediation-state record InfrastructureVoided — only when that record was
// written by this same run, still names the causes it charged, and is not an
// escalation. The next checkpoint refunds it (settleInfrastructureVoidedCycle).
// Best-effort like the rest of the failed handler: an error is journaled by
// the runner and never blocks the terminal, and a record this cannot update
// simply keeps today's charge.
func voidRemediationChargeForRun(ctx context.Context, poster gate.Commenter, l instance.Layout, runID string) error {
	items, err := claimedItemsForRun(l, runID)
	if err != nil {
		return fmt.Errorf("resolve claimed pull requests to void remediation charge: %w", err)
	}
	return markRemediationCharges(ctx, poster, items, runID, func(state *remediationState) {
		state.InfrastructureVoided = true
	})
}

// markRemediationCharges applies mark to the latest remediation-state record
// of every pull request in items, when that record is a cycle runID charged
// and nothing has settled it yet (remediationChargeVoidable). It is the one
// writer both provisional-charge refunds share: the infrastructure void above
// and the superseded-run refund (#6019, settleSupersededRun).
func markRemediationCharges(ctx context.Context, poster gate.Commenter, items []claimedItem, runID string, mark func(*remediationState)) error {
	var errs []error
	for _, item := range items {
		if item.Kind != itemKindPullRequest {
			continue
		}
		comments, err := poster.ListComments(ctx, item.Repo, item.ItemID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list comments on %s#%s: %w", item.Repo.Name, item.ItemID, err))
			continue
		}
		state, commentID, found := latestRemediationState(comments)
		if !found || commentID == "" || !remediationChargeVoidable(state, runID) {
			continue
		}
		mark(&state)
		if err := poster.UpdateComment(ctx, item.Repo, commentID, renderRemediationComment(state)); err != nil {
			errs = append(errs, fmt.Errorf("void remediation charge on %s#%s: %w", item.Repo.Name, item.ItemID, err))
		}
	}
	return errors.Join(errs...)
}

// remediationChargeVoidable reports whether state is an advancing cycle this
// run charged and has not already been voided or refunded as superseded.
func remediationChargeVoidable(state remediationState, runID string) bool {
	return runID != "" && state.RunID == runID && !state.Escalated &&
		!state.InfrastructureVoided && !state.Superseded && len(state.ChargedCauses) > 0
}
