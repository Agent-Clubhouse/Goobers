package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// queueWithdrawal decides, on every poll of a PR queue-watch is watching,
// whether the PR is withdrawn from landing (#6134): by an opt-out label, by a
// route to remediation that took it off landing, or by open human review
// feedback that arrived after the landing was armed. The last check is
// #7409's merge-pr gate — the same classifier, open-feedback set and
// blockUnansweredFeedback knob (on by default) — run against a queued GitHub
// PR or an armed Azure DevOps auto-complete, which would otherwise land the
// moment the queue or the branch policies settle.
type queueWithdrawal struct {
	gate mergeFeedbackGate
	// route takes a PR withdrawn for feedback off landing and routes it to
	// remediation (needs-remediation added, merge-ready removed).
	route func(ctx context.Context, repo providers.RepositoryRef, pullNumber, reason string) error
}

// reason returns why the PR is withdrawn, or "" while it keeps landing. The
// feedback is read once per poll and only while the PR is still queued or
// armed (Pending). A read that fails keeps the PR queued and is retried on the
// next poll: one failed read is not evidence of unanswered feedback, and the
// gate at merge time (#7409) already fails closed.
func (w queueWithdrawal) reason(ctx context.Context, pullNumber string, result providers.PollMergeQueueEntryResult, stderr io.Writer) string {
	if label := mergeQueueWithdrawLabel(result.Labels); label != "" {
		return label
	}
	if result.State != providers.MergeQueueEntryPending {
		return ""
	}
	unanswered, err := w.gate.assess(ctx, pullNumber)
	if err != nil {
		pf(stderr, "warning: pull request #%s: could not read its review feedback (%v); keeping it queued and re-reading on the next poll\n", pullNumber, err)
		return ""
	}
	return unanswered
}

// skipped reports a withdrawn PR. One withdrawn for unanswered feedback is
// also routed to remediation, with the typed reason in the result; it never
// counts toward demotion.
func (w queueWithdrawal) skipped(ctx context.Context, repo providers.RepositoryRef, pullNumber, withdrawn, resultFile string, stdout, stderr io.Writer) int {
	if !strings.HasPrefix(withdrawn, unansweredHumanFeedbackReasonCode) {
		return mergeQueuePollSkipped(pullNumber, withdrawn, resultFile, stdout, stderr)
	}
	if w.route == nil {
		return failProviderStage(stderr, "route the withdrawn pull request to remediation", fmt.Errorf("no routing transport"), resultFile)
	}
	if err := w.route(ctx, repo, pullNumber, withdrawn); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("route withdrawn pull request #%s to remediation", pullNumber), err, resultFile)
	}
	if err := writeQueueResult(resultFile, pullNumber, mergeReviewOptOutOutcome, "", nil, withdrawn); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue withdrew pr #%s over unanswered human review feedback; routed to remediation\n", pullNumber)
	return 0
}

// githubQueueFeedbackRoute labels a PR withdrawn from the GitHub merge queue
// for remediation with the stage's github:issues:write credential (the one
// eviction routing uses). Its explanatory comment carries a "none"
// feedback-ack frontier: it records the withdrawal and acknowledges no one's
// feedback.
func githubQueueFeedbackRoute(ctx context.Context, repo providers.RepositoryRef, pullNumber, reason string) error {
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		return err
	}
	provider, err := newMergeReviewProvider(providerStageRoot(""), repo, false,
		withStageProviderToken(token), withStageProviderMutations("pr"))
	if err != nil {
		return err
	}
	comment := withFeedbackAck("Withdrawn from the merge queue: open review feedback arrived after the landing was armed and has no answer from Goobers yet. Routed to remediation (`"+
		needsRemediationLabel+"`). Reason: "+reason, feedbackAckMarker(time.Time{}))
	_, err = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: pullNumber, AddLabels: []string{needsRemediationLabel}, RemoveLabels: []string{mergeReadyLabel}, Comment: comment,
	})
	return err
}

// adoQueueFeedbackRoute labels a PR whose auto-complete was cancelled for
// feedback with pull-request labels (never the same-numbered work item).
func adoQueueFeedbackRoute(provider *providers.ADOProvider) func(context.Context, providers.RepositoryRef, string, string) error {
	return func(ctx context.Context, repo providers.RepositoryRef, pullNumber, _ string) error {
		if provider == nil {
			return fmt.Errorf("azure devops provider unavailable")
		}
		if err := provider.AddPullRequestLabels(ctx, repo, pullNumber, []string{needsRemediationLabel}); err != nil {
			return err
		}
		return provider.RemovePullRequestLabel(ctx, repo, pullNumber, mergeReadyLabel)
	}
}
