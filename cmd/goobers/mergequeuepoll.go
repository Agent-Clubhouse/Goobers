package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"time"

	"github.com/goobers/goobers/internal/boundedwait"
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
// Merged, evicted, timed out, and skipped are successful determinations (exit 0):
// a merged pull request gets the same branch cleanup merge-pr's direct path
// already does; evicted and timed-out pull requests are labeled
// goobers:needs-remediation with an explanatory comment before reporting the
// outcome. A skipped pull request acquired goobers:no-merge-review while
// queued and is dequeued without any comment or label mutation.
const mergeQueuePollHelp = "Usage: goobers merge-queue-poll [path]\n\n" +
	"Watch a pull request already enqueued to its repo's merge queue (issue\n" +
	"#758's Land, in merge-queue-enqueue policy) until the queue merges or\n" +
	"evicts it, or this stage's own poll times out. Declared inputs:\n" +
	"pullNumber (required), pollIntervalSeconds/pollMaxIntervalSeconds/\n" +
	"pollTimeoutSeconds (time.ParseDuration strings, default to\n" +
	"internal/executor's ci-poll defaults), resultFile (default\n" +
	"queue-result.json). An eviction or timeout applies\n" +
	"goobers:needs-remediation plus an explanatory comment before reporting\n" +
	"its queueOutcome — a failure to apply that trail is a stage failure,\n" +
	"not a swallowed warning. A live goobers:no-merge-review opt-out dequeues\n" +
	"the pull request and reports skipped without remediation. Exit codes:\n" +
	"0 = evaluated (merged, evicted, skipped, or still-pending-timeout —\n" +
	"see the result file's queueOutcome field),\n" +
	"1 = business error (missing capability/config,\n" +
	"provider failure), 2 = usage/IO error.\n"

func runMergeQueuePoll(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("merge-queue-poll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "merge-queue-poll")
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
	// Azure DevOps land oracle (ADO merge epic). Dispatch to the ADO
	// branch here so the GitHub-only merge-queue machinery below stays
	// unreachable on ADO: the opt-out DequeuePullRequest, the eviction/timeout
	// PR-as-work-item remediation labeling (UpdateWorkItem(ID: prNumber) —
	// wrong-object hazard on ADO, §8), and the concrete-*GitHubProvider branch
	// cleanup helper (mergeQueuePollMerged). Every GitHub path below is
	// byte-identical; the ADO behavior is a new branch reached only here.
	if repo.Provider == providers.ProviderADO {
		return runMergeQueuePollADO(root, repo, stdout, stderr)
	}
	provider, err := newProviderForStageAs[*providers.GitHubProvider](root, repo, false,
		withStageProviderCapability(capability.GitHubPRMerge),
		withStageProviderMutations("pr"),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

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
	// Never poll past the deadline the executor will kill this stage at
	// (issue #884). Without this clamp the default 30m poll runs inside a
	// stage the shell executor SIGKILLs at 10m: the loop never reaches its
	// own timeout branch, so it never writes queue-result.json, so
	// queue-gate reads a missing queueOutcome as fail and the whole
	// merge-review run is journaled as FAILED — for a pull request that
	// was in fact successfully enqueued and will very likely merge.
	// Reporting a working landing as a failure is worse than reporting it
	// late, so the poll budget yields to the stage budget.
	if clamped := boundedwait.MergeQueuePollBudget(stageTimeout()); timeout > clamped {
		pf(stderr, "note: poll timeout %s exceeds this stage's own budget; polling for %s instead\n", timeout, clamped)
		timeout = clamped
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	deadline := time.Now().Add(timeout)
	// An absent queue entry is how a real eviction presents — GitHub leaves
	// the pull request open and just removes it from the queue (#885) — but
	// it is ALSO how two entirely different, benign situations present, and
	// absence alone cannot tell the three apart:
	//
	//   1. the entry has not become visible yet after merge-pr's enqueue
	//      (propagation lag, before any entry has ever been seen), and
	//   2. the queue just MERGED the pull request, which removes the entry
	//      in the same instant (#924).
	//
	// entrySeen distinguishes (1): once an entry has been seen, absence can
	// no longer be pre-enqueue lag. absentSince handles (2), which entrySeen
	// cannot, because it looks identical to a real eviction on a single read.
	// PollMergeQueueEntry does check pr.Merged before reporting Absent, but
	// that read is not atomic: the entry is gone and `merged` has not yet
	// flipped true on the replica the query happened to land on, so the poll
	// returns Absent for a pull request that is in fact already on main.
	//
	// So absence is never conclusive on first sight — it must PERSIST. A real
	// eviction leaves the pull request open and unmerged indefinitely, so
	// absence persists trivially; a merge resolves to Merged on the very next
	// poll. Costing one extra poll interval to tell them apart is the whole
	// mechanism.
	entrySeen := false
	graceUntil := time.Now().Add(mergeQueueEntryGrace)
	// Length of the current unbroken streak of absent reads; reset by any
	// conclusive non-absent read.
	absentStreak := 0
	optedOut := false
	for attempt := 0; ; attempt++ {
		result, pollErr := provider.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: repo, PullID: pullNumber})
		if pollErr != nil && !providers.IsTransientError(pollErr) {
			return failProviderStage(stderr, "poll merge queue entry", pollErr, resultFile)
		}
		if pollErr == nil {
			// #2238: a PR already sitting in the native merge queue must be
			// dequeued the instant its originating run is cancelled mid-flight,
			// same as an operator applying goobers:no-merge-review — the queue
			// is a second merge authority and must not land a PR merge-pr would
			// now refuse.
			if hasAnyLabel(result.Labels, []string{noMergeReviewLabel, abortedRunLabel}) {
				optedOut = true
			}
			if optedOut {
				switch result.State {
				case providers.MergeQueueEntryMerged:
					return mergeQueuePollMerged(ctx, provider, repo, pullNumber, result.MergeSHA, resultFile, stdout, stderr)
				case providers.MergeQueueEntryPending:
					entrySeen = true
					absentStreak = 0
					if err := provider.DequeuePullRequest(ctx, providers.DequeuePullRequestRequest{
						Repository: repo, PullID: pullNumber, PullRequestNodeID: result.PullRequestNodeID,
					}); err == nil {
						return mergeQueuePollSkipped(pullNumber, resultFile, stdout, stderr)
					} else {
						pf(stderr, "warning: dequeue opted-out pull request #%s: %v; continuing to monitor and retry\n", pullNumber, err)
					}
				case providers.MergeQueueEntryEvicted:
					return mergeQueuePollSkipped(pullNumber, resultFile, stdout, stderr)
				case providers.MergeQueueEntryAbsent:
					absentStreak++
					if (entrySeen || !time.Now().Before(graceUntil)) && absentStreak >= mergeQueueAbsenceConfirmPolls {
						return mergeQueuePollSkipped(pullNumber, resultFile, stdout, stderr)
					}
					pf(stdout, "pr #%s opted out but its merge queue entry is not visible; waiting to dequeue or confirm absence\n", pullNumber)
				}
			} else {
				switch result.State {
				case providers.MergeQueueEntryMerged:
					return mergeQueuePollMerged(ctx, provider, repo, pullNumber, result.MergeSHA, resultFile, stdout, stderr)
				case providers.MergeQueueEntryEvicted:
					return mergeQueuePollEvicted(ctx, repo, pullNumber, resultFile, stdout, stderr)
				case providers.MergeQueueEntryPending:
					entrySeen = true
					// A conclusive non-absent read breaks the absence streak.
					absentStreak = 0
				case providers.MergeQueueEntryAbsent:
					absentStreak++
					switch {
					case !entrySeen && time.Now().Before(graceUntil):
						// No entry has ever been seen and we are still inside the
						// enqueue-propagation grace window: treat as pending.
					case absentStreak >= mergeQueueAbsenceConfirmPolls:
						// Absence held across independent reads. A merge landing
						// in the gap would have resolved to Merged by now, so this
						// is a real eviction.
						pf(stdout, "pr #%s is open and unmerged with no merge queue entry across %d polls — evicted\n", pullNumber, absentStreak)
						return mergeQueuePollEvicted(ctx, repo, pullNumber, resultFile, stdout, stderr)
					default:
						// First absent read of this streak. Do NOT commit to an
						// eviction yet — this is exactly what a successful merge
						// also looks like for an instant (#924). Poll again and
						// let the merge become visible if that is what happened.
						pf(stdout, "pr #%s has no merge queue entry; re-polling to confirm before calling it an eviction\n", pullNumber)
					}
				}
			}
		}
		if time.Now().After(deadline) {
			if optedOut {
				// Removal was never confirmed, so the entry may still be queued
				// and may still merge after this watcher exits. Record it for
				// post-merge reconciliation before reporting the failure:
				// otherwise a late merge lands with none of the follow-up the
				// normal path performs — branch cleanup, issue close-out,
				// sibling fan-out, unparking — and nothing is left pointing at
				// the PR to notice. Recording an entry that turns out never to
				// merge is harmless; reconciliation is keyed on the merge
				// actually happening.
				if err := recordPostMergeTimeout(root, repo, pullNumber, time.Now()); err != nil {
					pf(stderr, "error: record unconfirmed opt-out dequeue for reconciliation: %v\n", err)
					return 1
				}
				pf(stderr, "error: pull request #%s opted out, but its merge queue removal could not be confirmed before timeout\n", pullNumber)
				return 1
			}
			if err := recordPostMergeTimeout(root, repo, pullNumber, time.Now()); err != nil {
				pf(stderr, "error: record timed-out merge queue entry for reconciliation: %v\n", err)
				return 1
			}
			return mergeQueuePollTimedOut(ctx, repo, pullNumber, timeout, resultFile, stdout, stderr)
		}
		select {
		case <-ctx.Done():
			pf(stderr, "error: %v\n", ctx.Err())
			return 1
		case <-time.After(mergeQueuePollBackoff(interval, maxInterval, attempt)):
		}
	}
}

func mergeQueuePollSkipped(pullNumber, resultFile string, stdout, stderr io.Writer) int {
	reason := fmt.Sprintf("pull request #%s is labeled %s and was removed from merge-review landing", pullNumber, noMergeReviewLabel)
	if err := writeQueueResult(resultFile, pullNumber, mergeReviewOptOutOutcome, "", nil, reason); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue skipped pr #%s after %s opt-out\n", pullNumber, noMergeReviewLabel)
	return 0
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
func mergeQueuePollEvicted(ctx context.Context, repo providers.RepositoryRef, pullNumber, resultFile string, stdout, stderr io.Writer) int {
	reason := fmt.Sprintf("merge queue evicted pull request #%s: its combined build against the projected merge state failed", pullNumber)
	comment := fmt.Sprintf("The merge queue evicted this pull request — its combined build against the projected merge state failed. Labeling `%s` for remediation.", needsRemediationLabel)
	return mergeQueuePollNeedsRemediation(ctx, repo, pullNumber, "evicted", reason, comment, resultFile, stdout, stderr)
}

func mergeQueuePollTimedOut(ctx context.Context, repo providers.RepositoryRef, pullNumber string, timeout time.Duration, resultFile string, stdout, stderr io.Writer) int {
	reason := fmt.Sprintf("merge queue poll for pull request #%s timed out after %s while it was still pending", pullNumber, timeout)
	comment := fmt.Sprintf("Merge queue monitoring timed out after `%s` while this pull request was still pending. Labeling `%s` so a remediation selector or human can verify its queue state. Post-merge reconciliation will continue checking in case it lands later.", timeout, needsRemediationLabel)
	return mergeQueuePollNeedsRemediation(ctx, repo, pullNumber, "timeout", reason, comment, resultFile, stdout, stderr)
}

func mergeQueuePollNeedsRemediation(ctx context.Context, repo providers.RepositoryRef, pullNumber, outcome, reason, comment, resultFile string, stdout, stderr io.Writer) int {
	labelToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	labelProvider, err := newProviderForStage(providerStageRoot(""), repo, false,
		withStageProviderToken(labelToken),
		withStageProviderMutations("pr"),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := labelProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: pullNumber, AddLabels: []string{needsRemediationLabel}, Comment: comment,
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("label %s pull request for remediation", outcome), err, resultFile)
	}
	if err := writeQueueResult(resultFile, pullNumber, outcome, "", nil, reason); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue %s pr #%s, labeled %s\n", outcome, pullNumber, needsRemediationLabel)
	return 0
}

// writeQueueResult writes merge-queue-poll's declared result file's flat
// JSON — selectedNumber (always present), queueOutcome
// ("merged"/"evicted"/"timeout"/"skipped", always present — this stage always
// determines one of the four before returning, matching ci-poll's own
// "always succeeds at determining an outcome" philosophy), mergeSha (on
// merged), reason (on evicted or timeout), and headBranch/branchCleanup/
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
// mergeQueueEntryGrace bounds how long an absent merge queue entry is
// tolerated as "not visible yet" before it is read as an eviction (#885).
// It only applies before any entry has been seen: once one has, absence is
// immediately conclusive. Long enough to absorb GitHub's propagation lag
// between a successful enqueue and the entry appearing; short enough that a
// genuine eviction still routes to remediation well inside the stage's own
// poll budget.
const mergeQueueEntryGrace = 90 * time.Second

// mergeQueueAbsenceConfirmPolls is how many CONSECUTIVE polls must find the
// merge queue entry absent before that is read as an eviction (#924). Unlike
// mergeQueueEntryGrace it applies whether or not an entry has been seen,
// because the case it exists for happens after the entry has been seen: the
// queue merges the pull request, which removes the entry, and a poll landing
// before `merged` propagates to the replica it reads sees exactly what an
// eviction looks like.
//
// Expressed in polls rather than wall-clock deliberately. The quantity that
// actually matters is "never commit to an eviction on a single read"; a
// duration is only a proxy for it, and a proxy that silently stops holding
// whenever an operator retunes pollIntervalSeconds. Two polls also scales the
// right way on its own — a longer configured interval buys proportionally more
// propagation time — and keeps the guard meaningful at the sub-second intervals
// tests drive it at.
//
// The cost is bounded and one-sided. A real eviction leaves the pull request
// open and unmerged indefinitely, so its absence persists and it still routes
// to remediation one poll interval later — negligible against the stage's 25m
// budget. A merge resolves to Merged on the very next poll and is never
// mislabeled. The error is not symmetric: guessing eviction wrongly posts a
// "build failed" comment and a goobers:needs-remediation label onto a pull
// request that is already on main, and nothing downstream ever removes either.
const mergeQueueAbsenceConfirmPolls = 2

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

// mergeQueuePollBackoff returns a jittered duration between half and all of
// base<<attempt, with the exponential ceiling capped at max.
func mergeQueuePollBackoff(base, max time.Duration, attempt int) time.Duration {
	ceiling := base << attempt
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	floor := ceiling / 2
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
}

// runMergeQueuePollADO is merge-queue-poll's Azure DevOps land oracle
// (merge-wiring-plan §1d/§7-step-2, CONF-3 #2076). It watches the
// auto-complete-armed pull request merge-pr enqueued until ADO reports it
// completed (Merged) — the same landed-oracle role PollMergeQueueEntry serves
// on GitHub. The poll is routed through the capability-checked *Dispatcher
// (providers.NewDispatcher) so the concrete-*GitHubProvider helpers this file's
// GitHub path uses (branch cleanup, DequeuePullRequest, PR-as-work-item
// remediation labeling) stay unreachable on ADO.
//
// Completion (merge) authority rides on the ado:pr:complete capability
// (capability.ADOPRComplete), the ADO counterpart to github:pr:merge, resolved
// BEFORE the provider is constructed — mirroring how the GitHub path gates the
// poll on github:pr:merge — so this stage cannot silently acquire completion
// authority from an ordinary ado:pr:write grant (pr-lifecycle-loop §7,
// decider≠executor).
//
// PERIPHERAL GitHub side effects are documented no-ops on ADO:
//   - opt-out dequeue (DequeuePullRequest, §1d:166): no ADO equivalent — the
//     "queue" is ADO's own auto-complete, not a separate merge authority to
//     dequeue from. ADO PollMergeQueueEntry does populate Labels, but there is
//     nothing to dequeue, so no opt-out handling runs here.
//   - eviction/timeout remediation (§1d:294-324, §8): GitHub routes an evicted
//     or timed-out PR to pr-remediation via UpdateWorkItem(ID: prNumber,
//     AddLabels…). On ADO that numeric PR id addresses wit/workitems/{id},
//     mutating an unrelated work item — the PR-as-work-item hazard — so the
//     outcome is recorded to the result file WITHOUT any work-item write.
//   - branch cleanup (§1d:66): ADO PollPullRequest never reports
//     HeadRepository, so cleanupMergedBranch cannot run; skipped.
//
// Wiring the ADO remediation-routing and branch-cleanup follow-ups is deferred
// to the ADO merge epic (CONF-3 #2076).
func runMergeQueuePollADO(root string, repo providers.RepositoryRef, stdout, stderr io.Writer) int {
	// Completion authority is a distinct capability from ordinary
	// ado:pr:write. Resolve the grant before constructing the provider so an
	// un-granted stage fails closed rather than completing a pull request.
	if _, err := providerToken(capability.ADOPRComplete); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	adoProvider, err := newProviderForStageAs[*providers.ADOProvider](root, repo, false)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	dispatcher := providers.NewDispatcher(adoProvider)

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
	// Same stage-budget clamp the GitHub path applies (#884): never poll past
	// the deadline the executor SIGKILLs this stage at.
	if clamped := boundedwait.MergeQueuePollBudget(stageTimeout()); timeout > clamped {
		pf(stderr, "note: poll timeout %s exceeds this stage's own budget; polling for %s instead\n", timeout, clamped)
		timeout = clamped
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		result, pollErr := dispatcher.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: repo, PullID: pullNumber})
		if pollErr != nil && !providers.IsTransientError(pollErr) {
			return failProviderStage(stderr, "poll merge queue entry", pollErr, resultFile)
		}
		if pollErr == nil {
			// ADO reports explicit terminal states (completed→Merged,
			// abandoned/auto-complete-cleared→Evicted) — there is no GitHub-style
			// absent-entry ambiguity to disambiguate, so Pending simply keeps
			// polling until a terminal state or this stage's own timeout.
			switch result.State {
			case providers.MergeQueueEntryMerged:
				return mergeQueuePollMergedADO(pullNumber, result.MergeSHA, resultFile, stdout, stderr)
			case providers.MergeQueueEntryEvicted:
				return mergeQueuePollEvictedADO(pullNumber, resultFile, stdout, stderr)
			case providers.MergeQueueEntryPending, providers.MergeQueueEntryAbsent:
				// Still auto-completing; keep watching.
			}
		}
		if time.Now().After(deadline) {
			return mergeQueuePollTimedOutADO(pullNumber, timeout, resultFile, stdout, stderr)
		}
		select {
		case <-ctx.Done():
			pf(stderr, "error: %v\n", ctx.Err())
			return 1
		case <-time.After(mergeQueuePollBackoff(interval, maxInterval, attempt)):
		}
	}
}

// mergeQueuePollMergedADO reports an ADO-completed pull request as merged. The
// merged determination itself is the CORE land action and is written unchanged;
// branch cleanup (cleanupMergedBranch, the GitHub concrete-*GitHubProvider
// helper) is a documented no-op on ADO because ADO PollPullRequest never
// reports HeadRepository (merge-wiring-plan §1d/§8), so it is skipped rather
// than run against a nil head repository.
func mergeQueuePollMergedADO(pullNumber, mergeSHA, resultFile string, stdout, stderr io.Writer) int {
	if err := writeQueueResult(resultFile, pullNumber, "merged", mergeSHA, nil, ""); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue merged pr #%s (%s); branch cleanup deferred on ado\n", pullNumber, mergeSHA)
	return 0
}

// mergeQueuePollEvictedADO records an ADO queue eviction as an outcome WITHOUT
// the GitHub remediation-labeling side effect. On GitHub an eviction is routed
// to pr-remediation by UpdateWorkItem(ID: prNumber, AddLabels…); on ADO that
// numeric PR id addresses wit/workitems/{id}, so the write would mutate an
// unrelated work item (PR-as-work-item hazard, merge-wiring-plan §1d/§8). The
// outcome is written so queue-gate can still read it; the remediation routing
// is deferred to the ADO merge epic (CONF-3 #2076).
func mergeQueuePollEvictedADO(pullNumber, resultFile string, stdout, stderr io.Writer) int {
	reason := fmt.Sprintf("merge queue evicted pull request #%s: its build against the projected merge state failed; ado remediation labeling deferred to the ADO merge epic (CONF-3 #2076)", pullNumber)
	if err := writeQueueResult(resultFile, pullNumber, "evicted", "", nil, reason); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue evicted pr #%s; remediation labeling skipped on ado (pr-as-work-item hazard)\n", pullNumber)
	return 0
}

// mergeQueuePollTimedOutADO records an ADO poll timeout without the GitHub
// remediation-labeling side effect (same PR-as-work-item hazard as
// mergeQueuePollEvictedADO). It also skips recordPostMergeTimeout: post-merge
// reconciliation is GitHub-hardwired (merge-wiring-plan §0), so seeding it with
// an ADO entry is out of scope for this epic and deferred alongside the ADO
// remediation routing (CONF-3 #2076).
func mergeQueuePollTimedOutADO(pullNumber string, timeout time.Duration, resultFile string, stdout, stderr io.Writer) int {
	reason := fmt.Sprintf("merge queue poll for pull request #%s timed out after %s while it was still pending; ado remediation labeling deferred to the ADO merge epic (CONF-3 #2076)", pullNumber, timeout)
	if err := writeQueueResult(resultFile, pullNumber, "timeout", "", nil, reason); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "merge queue poll for pr #%s timed out; remediation labeling skipped on ado (pr-as-work-item hazard)\n", pullNumber)
	return 0
}
