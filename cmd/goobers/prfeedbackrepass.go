package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
)

// prfeedbackrepass.go classifies a stale-feedback repass (#6126).
//
// Stale input is rejected and re-gathered: guard-before-push or
// resolve-review-threads reports staleInput, the workflow re-enters
// gather-review-threads, and the agent answers the fresh snapshot. Often the
// new feedback needs no code change ("thanks", a question the thread reply
// answers). The agent then commits nothing, and the branch is still exactly
// the head that already passed review and local CI (pre-publication) or that
// this run already published. Sending that unchanged head back through the
// reviewer would trip its identical-diff guard (UNCHANGED_REPASS) and park
// the PR, and push-remediated would refuse it as "nothing to publish" — a
// casual comment would escalate a PR that is fine.
//
// pr-claim --classify-feedback-repass runs right after the agent's repass and
// answers one question from the run journal and the workspace: is this a
// stale-feedback repass whose agent pass left the branch at the reference
// head the stale check recorded? If so it reports feedbackNoop=true, and the
// workflow skips review and local CI (both already passed on this exact head)
// and goes straight back to guard-before-push, which re-verifies the feedback
// against the fresh snapshot before anything is published. Every other case —
// a first pass, a reviewer or CI repass, an agent that did change something,
// or no recorded reference head — reports feedbackNoop=false and takes the
// normal review path unchanged.

const (
	feedbackNoopOutput          = "feedbackNoop"
	feedbackReferenceHeadOutput = "referenceHeadSha"
	feedbackRepassCauseOutput   = "repassCause"
	// staleLocalHeadOutput is the workspace head guard-before-push records
	// beside a stale verdict: the head that had already passed review and
	// local CI when the feedback was found to have changed.
	staleLocalHeadOutput = "localHead"
	// stalePublishedHeadOutput is resolve-review-threads' record of the head
	// this run published, beside its stale verdict.
	stalePublishedHeadOutput = "publishedHeadSha"

	prFeedbackGatherStage = "gather-review-threads"
)

// feedbackRepassRecord is the newest stale-input verdict in a run's journal.
type feedbackRepassRecord struct {
	found      bool
	reason     string
	stage      string
	reference  string
	reGathered bool
}

// latestFeedbackRepass scans the run's events for the newest stage that
// reported a non-empty staleInput, the reference head it recorded, and
// whether gather-review-threads has run since (the re-gather that makes the
// current agent pass a feedback repass).
func latestFeedbackRepass(events []journal.Event) feedbackRepassRecord {
	var rec feedbackRepassRecord
	for _, event := range events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		if reason, _ := event.Outputs[staleInputOutput].(string); strings.TrimSpace(reason) != "" {
			rec = feedbackRepassRecord{found: true, reason: reason, stage: event.Stage}
			rec.reference = staleReferenceHead(event.Outputs)
			continue
		}
		if rec.found && event.Stage == prFeedbackGatherStage {
			rec.reGathered = true
		}
	}
	return rec
}

func staleReferenceHead(outputs map[string]any) string {
	for _, key := range []string{staleLocalHeadOutput, stalePublishedHeadOutput} {
		if head, _ := outputs[key].(string); strings.TrimSpace(head) != "" {
			return strings.TrimSpace(head)
		}
	}
	return ""
}

// classifyFeedbackRepass is pr-claim --classify-feedback-repass.
func classifyFeedbackRepass(root string, number int, stdout, stderr io.Writer) int {
	runID, _, err := providerRunContext()
	if err != nil {
		return failProviderStage(stderr, "read run context", err, prRemediationLifecycleResultFile)
	}
	var events []journal.Event
	rd, err := stageRunJournal(root, runID)
	switch {
	case errors.Is(err, journalclient.ErrRunNotFound):
	case err != nil:
		return failProviderStage(stderr, "read run journal", err, prRemediationLifecycleResultFile)
	default:
		if events, err = rd.Events(); err != nil {
			return failProviderStage(stderr, "read run journal", err, prRemediationLifecycleResultFile)
		}
	}
	rec := latestFeedbackRepass(events)
	localHead := ""
	if rec.found && rec.reGathered && rec.reference != "" {
		if localHead, err = resolveHead("."); err != nil {
			pf(stderr, "error: resolve local head for PR #%d: %v\n", number, err)
			return 1
		}
	}
	noop := localHead != "" && strings.EqualFold(localHead, rec.reference)
	payload := map[string]interface{}{
		"selectedNumber":            strconv.Itoa(number),
		feedbackNoopOutput:          strconv.FormatBool(noop),
		feedbackReferenceHeadOutput: rec.reference,
		staleLocalHeadOutput:        localHead,
	}
	if rec.found && rec.reGathered {
		// Deliberately not staleInput: this stage must never read as a stale
		// verdict itself, or the next classification would find it.
		payload[feedbackRepassCauseOutput] = rec.reason
	}
	if err := writeProviderStagePayload(providerInput("resultFile", prRemediationLifecycleResultFile), payload); err != nil {
		pf(stderr, "error: write pr-claim result: %v\n", err)
		return 2
	}
	switch {
	case noop:
		pf(stdout, "PR #%d: feedback acknowledged, no change needed — the %s repass left the branch at %s, which %s already accepted; skipping review and local CI\n",
			number, rec.reason, localHead, rec.stage)
	case rec.found && rec.reGathered:
		pf(stdout, "PR #%d: the %s repass changed the branch; it goes through review and local CI\n", number, rec.reason)
	default:
		pf(stdout, "PR #%d: not a stale-feedback repass\n", number)
	}
	return 0
}

// workspaceIsRepository reports whether the stage runs at the root of a git
// checkout (the run's worktree), as opposed to a scratch workspace — where
// `git rev-parse HEAD` would walk up into whatever repository happens to
// contain it.
func workspaceIsRepository() bool {
	_, err := os.Stat(".git")
	return err == nil
}

// recordStaleLocalHead adds the workspace head to a stale --verify-feedback
// result, so a later classify-feedback-repass can tell whether the repass
// changed anything. Outside a repository it records nothing, which keeps the
// repass on the normal review path.
func recordStaleLocalHead(result *prRemediationLifecycleResult, stderr io.Writer) {
	if result.StaleInput == "" || !workspaceIsRepository() {
		return
	}
	head, err := resolveHead(".")
	if err != nil {
		pf(stderr, "warning: %v\n", fmt.Errorf("record local head beside stale feedback: %w", err))
		return
	}
	result.LocalHead = head
}
