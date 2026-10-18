package main

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// feedbackCheck is one live comparison against the run's recorded snapshot.
type feedbackCheck struct {
	// recorded is false when the brief carries no snapshot (a run gathered
	// before v4): there is nothing to compare, and the check passes.
	recorded bool
	snapshot apiv1.PRFeedbackSnapshot
	live     apiv1.PRFeedbackSnapshot
	evidence feedbackEvidence
	reasons  []feedbackStaleReason
}

func (c feedbackCheck) stale() bool { return len(c.reasons) > 0 }

// checkLiveFeedback re-reads every feedback source and compares it with the
// recorded snapshot. A provider error that the provider classification calls
// retryable is returned as an error (the stage's infrastructure retry owns
// it); any other read failure — a torn read that raced a head change, or a
// listing a provider could not assemble consistently — becomes a stale reason,
// because an incomplete read must never be mistaken for a clean one.
func checkLiveFeedback(ctx context.Context, src prFeedbackSource, repo providers.RepositoryRef, recorded *apiv1.PRFeedbackSnapshot, opts feedbackCompareOptions) (feedbackCheck, error) {
	if recorded == nil {
		return feedbackCheck{}, nil
	}
	check := feedbackCheck{recorded: true, snapshot: *recorded}
	ev, err := readFeedbackEvidence(ctx, src, repo, recorded.PullRequest)
	check.evidence = ev
	switch {
	case errors.Is(err, errFeedbackTorn):
		check.reasons = []feedbackStaleReason{{Code: staleReasonHead, Kind: "head", Detail: err.Error()}}
		return check, nil
	case err != nil:
		if _, retryable, _ := classifyProviderError(err); retryable {
			return check, err
		}
		check.reasons = []feedbackStaleReason{{Code: staleReasonIncomplete, Kind: "collection", Detail: err.Error()}}
		return check, nil
	}
	check.live = buildFeedbackSnapshot(repo, recorded.PullRequest, ev, time.Now())
	check.reasons = compareFeedbackSnapshot(*recorded, check.live, opts)
	return check, nil
}

// checkThreadFeedback compares just the review-thread half of the snapshot
// with a listing a stage already fetched (resolve-review-threads re-lists
// threads around every mutation), so per-mutation checks cost no extra call.
func checkThreadFeedback(recorded *apiv1.PRFeedbackSnapshot, listing providers.PullRequestReviewThreads, opts feedbackCompareOptions) []feedbackStaleReason {
	if recorded == nil {
		return nil
	}
	live := *recorded
	live.Reviews = canonicalReviews(listing.Reviews)
	live.ReviewThreads = canonicalThreads(listing.InlineComments)
	opts.expectedHead = recorded.HeadSHA
	live.HeadSHA = recorded.HeadSHA
	return compareFeedbackSnapshot(*recorded, live, opts)
}
