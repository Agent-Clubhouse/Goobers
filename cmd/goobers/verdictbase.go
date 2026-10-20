package main

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

type verdictBasePublisher interface {
	providers.MergeReviewStatusReader
	providers.PullRequestStatusPublisher
}

type verdictBaseReconciler struct {
	root      string
	repo      providers.RepositoryRef
	reader    remediationProvider
	publisher verdictBasePublisher
}

// Legacy workflows without the review credential retain their old admission
// contract. Shipped workflows declare it; the absence is visible and does not
// advance the withdrawal cursor or substitute a different publishing identity.
func newVerdictBaseReconciler(root string, repo providers.RepositoryRef, reader remediationProvider, stderr io.Writer) *verdictBaseReconciler {
	publisher, err := newVerdictStatusPublisher(root, repo)
	if err != nil {
		pf(stderr, "warning: native base-verdict reconciliation unavailable; declare github:pr:review with commit-status permission\n")
		return nil
	}
	native, ok := publisher.(verdictBasePublisher)
	if !ok {
		return nil
	}
	return &verdictBaseReconciler{root: root, repo: repo, reader: reader, publisher: native}
}

// reconcile rereads both the PR and native verdict under publication custody.
// A previously queued withdrawal therefore observes, and preserves, a fresh
// pass published for the current base. Only a readable standing pass is changed.
func (r *verdictBaseReconciler) reconcile(ctx context.Context, number int, stderr io.Writer) error {
	return withVerdictPublicationCustody(ctx, r.root, r.repo, func(ctx context.Context) error {
		pull := strconv.Itoa(number)
		live, err := r.reader.GetPullRequest(ctx, r.repo, pull)
		if err != nil {
			return err
		}
		if live.Merged || live.State == "closed" {
			return nil
		}
		status, err := r.publisher.MergeReviewStatus(ctx, r.repo, live.HeadSHA)
		if err != nil {
			return err
		}
		if status == nil || status.State != providers.CheckStatePassing {
			return nil
		}
		decision, reviewedBase, parsed := prstatus.ParseVerdictDescription(status.Summary)
		if parsed && decision == "pass" && reviewedBase == live.BaseSHA {
			return nil
		}
		reason := "standing pass has no valid reviewed base"
		if parsed && decision == "pass" {
			intersects, compareErr := prstatus.BaseMovementIntersectsPR(ctx, r.reader, r.repo, pull, reviewedBase, live.BaseSHA)
			if compareErr == nil && !intersects {
				return nil
			}
			reason = "base movement invalidated the reviewed files"
			if compareErr != nil {
				reason = "base movement could not be verified as disjoint"
			}
		}
		// Provider reads are not atomic with base/head movement. Do not publish
		// against a different subject from the one whose delta was just checked.
		checked, err := r.reader.GetPullRequest(ctx, r.repo, pull)
		if err != nil {
			return err
		}
		if checked.HeadSHA != live.HeadSHA || checked.BaseSHA != live.BaseSHA {
			return fmt.Errorf("PR #%d moved during base-verdict reconciliation; retry", number)
		}
		result, err := prstatus.WithdrawVerdict(ctx, r.publisher, prstatus.StaleVerdictStatusRequest(r.repo, pull, live.HeadSHA, live.BaseSHA, reason, live.URL))
		if err != nil {
			return err
		}
		if result.Warning != "" {
			return fmt.Errorf("withdraw PR #%d verdict: %s", number, result.Warning)
		}
		if !result.Unchanged {
			pf(stderr, "PR #%d: withdrew standing merge-review pass after base movement\n", number)
		}
		return nil
	})
}

func withdrawSiblingBaseVerdicts(ctx context.Context, root string, provider remediationProvider, repo providers.RepositoryRef, merged int, others []providers.PullRequestSummary, stderr io.Writer) []error {
	r := newVerdictBaseReconciler(root, repo, provider, stderr)
	if r == nil {
		return nil
	}
	var errs []error
	for _, pr := range others {
		if pr.Number == merged {
			continue
		}
		if err := r.reconcile(ctx, pr.Number, stderr); err != nil {
			errs = append(errs, fmt.Errorf("reconcile native verdict for PR #%d: %w", pr.Number, err))
		}
	}
	return errs
}
