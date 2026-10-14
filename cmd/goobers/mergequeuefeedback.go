package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// queueWithdrawal decides, on every poll of a PR queue-watch is watching,
// whether the PR is withdrawn from landing (#6134): by an opt-out label, by a
// route to remediation that took it off landing, or by open human review
// feedback that arrived after the landing was armed. The last check is
// #7409's merge-pr gate — the same classifier, open-feedback set, "self"
// (merge-pr's verdictAuthor, bound via inputsFrom) and blockUnansweredFeedback
// knob (on by default) — run against a queued GitHub PR or an armed Azure
// DevOps auto-complete, which would otherwise land the moment the queue or the
// branch policies settle.
type queueWithdrawal struct {
	gate mergeFeedbackGate
	// provider builds the PR-labelling surface the refusal routing uses (the
	// same one record-merge-refusal routes merge-pr's refusals through).
	provider func() (remediationProvider, error)
}

// reason returns why the PR is withdrawn, or "" while it keeps landing. The
// feedback is read once per poll and only while the PR is still queued or
// armed (Pending). A transient or transport read failure keeps the PR queued
// and is retried on the next poll: one failed read is not evidence of
// feedback, and merge-pr's gate already failed closed before arming. A
// deterministic one — an invalid setting on this stage, or an Azure DevOps
// comment whose author or time cannot be read — withdraws the PR as
// human-feedback-unverifiable, as merge-pr would refuse it.
func (w queueWithdrawal) reason(ctx context.Context, pullNumber string, result providers.PollMergeQueueEntryResult, stderr io.Writer) string {
	if label := mergeQueueWithdrawLabel(result.Labels); label != "" {
		return label
	}
	if result.State != providers.MergeQueueEntryPending {
		return ""
	}
	unanswered, err := w.gate.assess(ctx, pullNumber)
	if err == nil {
		return unanswered
	}
	if w.gate.err != nil || errors.Is(err, providers.ErrUnreadableFeedbackComment) {
		return humanFeedbackUnverifiableReason(err)
	}
	pf(stderr, "warning: pull request #%s: could not read its review feedback (%v); keeping it queued and re-reading on the next poll\n", pullNumber, err)
	return ""
}

// skipped reports a withdrawn PR. A withdrawal for unanswered feedback goes
// through record-merge-refusal's routing (routeUnansweredFeedbackRefusal): the
// first refusal over a comment routes the PR to remediation, a repeat parks
// it for a human, and neither counts toward demotion. An unverifiable
// withdrawal parks the PR for a human directly: remediation cannot fix an
// invalid setting or an unreadable comment.
func (w queueWithdrawal) skipped(ctx context.Context, repo providers.RepositoryRef, pullNumber, withdrawn, resultFile string, stdout, stderr io.Writer) int {
	feedback := strings.HasPrefix(withdrawn, unansweredHumanFeedbackReasonCode)
	unverifiable := strings.HasPrefix(withdrawn, humanFeedbackUnverifiableReasonCode)
	if !feedback && !unverifiable {
		return mergeQueuePollSkipped(pullNumber, withdrawn, resultFile, stdout, stderr)
	}
	number, err := strconv.Atoi(pullNumber)
	if err != nil {
		return failProviderStage(stderr, "route the withdrawn pull request", fmt.Errorf("invalid pull number %q", pullNumber), resultFile)
	}
	if w.provider == nil {
		return failProviderStage(stderr, "route the withdrawn pull request", fmt.Errorf("no routing provider"), resultFile)
	}
	provider, err := w.provider()
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("route withdrawn pull request #%s", pullNumber), err, resultFile)
	}
	if feedback {
		if code := routeUnansweredFeedbackRefusal(ctx, provider, repo, number, "", withdrawn, stdout, stderr); code != 0 {
			return code
		}
	} else if err := parkUnverifiableFeedback(ctx, provider, repo, number, withdrawn); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("park withdrawn pull request #%s", pullNumber), err, resultFile)
	}
	if err := writeQueueResult(resultFile, pullNumber, mergeReviewOptOutOutcome, "", nil, withdrawn); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue withdrew pr #%s: %s\n", pullNumber, withdrawn)
	return 0
}

// parkUnverifiableFeedback parks a PR whose feedback could not be verified
// for a human: goobers:needs-human added, goobers:merge-ready removed, with a
// note that acknowledges no one's feedback ("none" frontier).
func parkUnverifiableFeedback(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, number int, reason string) error {
	transport := newFeedbackRefusalTransport(provider, repo, number)
	if err := transport.labels(ctx, []string{providers.LabelNeedsHuman}, []string{mergeReadyLabel}); err != nil {
		return err
	}
	body := "**Withdrawn from landing and parked for a human** (`" + providers.LabelNeedsHuman + "`): the pull request's review " +
		"feedback could not be verified, so it must not land unseen. Fix the cause below, then remove the label.\n\nReason: " + reason
	return transport.post(ctx, withFeedbackAck(body, feedbackAckMarker(time.Time{})))
}
