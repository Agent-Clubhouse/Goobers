package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// runMergeQueuePoll implements the `goobers merge-queue-poll` built-in
// stage kind (issue #758): the queue-eviction-as-a-first-class-outcome half
// of the merge-policy abstraction. merge-pr's enqueue-policy Land dispatch
// only adds a pull request to its repo's merge queue; this stage watches
// what the queue does with it next — merges it, evicts it, or (bounded,
// like ci-poll — internal/executor/cipoll.go's own doc) neither happens
// before this stage's own poll times out.
//
// Merged and evicted are both terminal, successful determinations (exit 0):
// a merged pull request gets the same branch cleanup merge-pr's direct path
// already does; an evicted one is labeled goobers:needs-remediation with an
// explanatory comment before reporting the outcome — the routing IS the
// acceptance criterion, so a failure to apply that label is a genuine stage
// failure (exit 1), not a swallowed warning that would silently leave an
// evicted pull request unrouted. A still-pending entry past this stage's
// own poll timeout is also exit 0 (queueOutcome=timeout) — mergepr.go's own
// "not ready yet is not a stage failure" philosophy, not ci-poll's
// executor-kind ResultFailure/Retryable convention (this is a plain
// provider-chain subcommand, not that distinct executor path).
func runMergeQueuePoll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("merge-queue-poll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers merge-queue-poll [path]\n\n"+
			"Watch a pull request already enqueued to its repo's merge queue (issue\n"+
			"#758's Land, in merge-queue-enqueue policy) until the queue merges or\n"+
			"evicts it, or this stage's own poll times out. Declared inputs:\n"+
			"pullNumber (required), pollIntervalSeconds/pollMaxIntervalSeconds/\n"+
			"pollTimeoutSeconds (time.ParseDuration strings, default to\n"+
			"internal/executor's ci-poll defaults), resultFile (default\n"+
			"queue-result.json). An eviction applies goobers:needs-remediation plus\n"+
			"an explanatory comment before reporting queueOutcome=evicted — that\n"+
			"labeling is the acceptance criterion, so a failure to apply it is a\n"+
			"stage failure, not a swallowed warning. Exit codes: 0 = evaluated\n"+
			"(merged, evicted, or still-pending-timeout — see the result file's\n"+
			"queueOutcome field), 1 = business error (missing capability/config,\n"+
			"provider failure), 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubPRMerge)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))

	pullNumber := providerInput("pullNumber", "")
	if pullNumber == "" {
		pf(stderr, "error: pullNumber input is required\n")
		return 1
	}
	resultFile := providerInput("resultFile", "queue-result.json")
	interval, err := pollDurationInput("pollIntervalSeconds", executor.DefaultPollInterval)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	maxInterval, err := pollDurationInput("pollMaxIntervalSeconds", executor.DefaultMaxPollInterval)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	timeout, err := pollDurationInput("pollTimeoutSeconds", executor.DefaultPollTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		result, pollErr := provider.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: repo, PullID: pullNumber})
		if pollErr != nil && !providers.IsTransientError(pollErr) {
			return failProviderStage(stderr, "poll merge queue entry", pollErr, resultFile)
		}
		if pollErr == nil {
			switch result.State {
			case providers.MergeQueueEntryMerged:
				return mergeQueuePollMerged(ctx, provider, repo, pullNumber, result.MergeSHA, resultFile, stdout, stderr)
			case providers.MergeQueueEntryEvicted:
				return mergeQueuePollEvicted(ctx, provider, repo, pullNumber, resultFile, stdout, stderr)
			}
			// MergeQueueEntryPending falls through to the backoff/timeout
			// check below, same as a transient poll error does.
		}
		if time.Now().After(deadline) {
			if err := writeQueueResult(resultFile, pullNumber, "timeout", "", nil, ""); err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			pf(stdout, "merge queue poll for pr #%s timed out after %s, still pending\n", pullNumber, timeout)
			return 0
		}
		select {
		case <-ctx.Done():
			pf(stderr, "error: %v\n", ctx.Err())
			return 1
		case <-time.After(mergeQueuePollBackoff(interval, maxInterval, attempt)):
		}
	}
}

// mergeQueuePollMerged reports a queue-merged pull request and runs the
// same branch cleanup merge-pr's direct-merge path already does — a
// separate PollPullRequest call resolves the head branch/repository
// PollMergeQueueEntryResult does not itself carry.
func mergeQueuePollMerged(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pullNumber, mergeSHA, resultFile string, stdout, stderr io.Writer) int {
	var cleanup *mergeBranchCleanup
	poll, pollErr := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
	if pollErr != nil {
		pf(stderr, "warning: merge queue merged pr #%s but branch cleanup lookup failed: %v\n", pullNumber, pollErr)
	} else {
		outcome := cleanupMergedBranch(ctx, poll.HeadRepository, poll.HeadBranch, provider)
		cleanup = &outcome
		if outcome.Error != "" {
			pf(stderr, "warning: merge queue merged pr #%s but branch cleanup failed: %s\n", pullNumber, outcome.Error)
		} else {
			pf(stdout, "branch cleanup %s (%s)\n", outcome.Status, outcome.HeadBranch)
		}
	}
	if err := writeQueueResult(resultFile, pullNumber, "merged", mergeSHA, cleanup, ""); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue merged pr #%s (%s)\n", pullNumber, mergeSHA)
	return 0
}

// mergeQueuePollEvicted labels an evicted pull request goobers:needs-
// remediation with an explanatory comment (issue #758's "queue eviction
// routes to remediation as an explicit outcome" acceptance criterion) —
// the same UpdateWorkItem mechanism postmerge.go's fan-out already uses to
// route a PR into pr-remediation's own selection tiering
// (remediationPriorityNeedsRemediation), so an evicted PR needs no new
// downstream plumbing to be picked up. A dedicated, narrowly-scoped token
// (capability.GitHubIssuesWrite), resolved lazily only when actually
// needed — mirroring cleanupMergedBranch's own GitHubBranchDelete pattern —
// since labeling is a distinct authority from the github:pr:merge token
// this stage's poll itself runs under.
func mergeQueuePollEvicted(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pullNumber, resultFile string, stdout, stderr io.Writer) int {
	labelToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	labelProvider := newGitHubProvider(labelToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
	reason := fmt.Sprintf("merge queue evicted pull request #%s: its combined build against the projected merge state failed", pullNumber)
	comment := fmt.Sprintf("The merge queue evicted this pull request — its combined build against the projected merge state failed. Labeling `%s` for remediation.", needsRemediationLabel)
	if _, err := labelProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: pullNumber, AddLabels: []string{needsRemediationLabel}, Comment: comment,
	}); err != nil {
		return failProviderStage(stderr, "label evicted pull request for remediation", err, resultFile)
	}
	if err := writeQueueResult(resultFile, pullNumber, "evicted", "", nil, reason); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue evicted pr #%s, labeled %s\n", pullNumber, needsRemediationLabel)
	return 0
}

// writeQueueResult writes merge-queue-poll's declared result file's flat
// JSON — selectedNumber (always present), queueOutcome
// ("merged"/"evicted"/"timeout", always present — this stage always
// determines one of the three before returning, matching ci-poll's own
// "always succeeds at determining an outcome" philosophy), mergeSha (on
// merged), reason (on evicted), and headBranch/branchCleanup/
// branchCleanupError (after a merge) — the same flat-scalar convention
// writeMergeResult already follows.
func writeQueueResult(path, selectedNumber, queueOutcome, mergeSHA string, cleanup *mergeBranchCleanup, reason string) error {
	out := map[string]interface{}{"selectedNumber": selectedNumber, "queueOutcome": queueOutcome}
	if mergeSHA != "" {
		out["mergeSha"] = mergeSHA
	}
	if reason != "" {
		out["reason"] = reason
	}
	if cleanup != nil {
		out["branchCleanup"] = cleanup.Status
		out["headBranch"] = cleanup.HeadBranch
		if cleanup.Error != "" {
			out["branchCleanupError"] = cleanup.Error
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal queue result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// pollDurationInput reads a declared time.ParseDuration-string input
// (e.g. "15s"), defaulting to def when unset — mirroring
// internal/executor/cipoll.go's durationInput: an unset key applies the
// caller's default, but a SET, malformed value fails closed with a real
// error rather than silently defaulting.
func pollDurationInput(key string, def time.Duration) (time.Duration, error) {
	v := providerInput(key, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s input %q: %w", key, v, err)
	}
	return d, nil
}

// mergeQueuePollBackoff returns base<<attempt capped at max — this
// package's own copy of internal/executor/cipoll.go's unexported backoff,
// for the same capped-exponential poll cadence.
func mergeQueuePollBackoff(base, max time.Duration, attempt int) time.Duration {
	d := base << attempt
	if d <= 0 || d > max {
		return max
	}
	return d
}
