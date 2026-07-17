package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/providers"
)

// runMergePR implements the `goobers merge-pr` built-in stage kind (issue
// #360): the provider-level conjunctive auto-merge action `merge-review`
// drives. It merges a PR only when EVERY independent conjunct holds —
// verdict=pass, CI green, not a draft, and the SHA-pin (headSha/baseSha)
// still matches the PR's LIVE state — never a bare self-approval, and never
// trusting a caller-supplied "still valid" claim instead of re-polling
// (docs/design/v0/pr-lifecycle-loop.md §7/D6).
//
// A PR missing any one conjunct is a normal, expected outcome (the PR just
// isn't ready yet), not a stage failure: it exits 0 with merged=false and a
// human-readable reason in the declared result file, so a downstream gate
// can branch on Outputs["merged"] — the same philosophy as ci-poll, whose
// stage always succeeds at determining an outcome even when that outcome is
// "still pending" (internal/executor/cipoll.go's ciPollOutcome doc). Only a
// genuine provider/config error (missing capability, unresolvable repo, a
// merge attempt that should have succeeded but didn't) is a business error.
func runMergePR(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("merge-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers merge-pr [path]\n\n"+
			"Merge a pull request, but only when every independent conjunct holds:\n"+
			"verdict=pass, CI green, not a draft, and the SHA-pin still matches the\n"+
			"PR's live head/base (never a bare self-approval). Declared inputs:\n"+
			"pullNumber, verdict, headSha, baseSha (all required), advisoryMode\n"+
			"(default false — report only, no merge attempted), mergeMethod\n"+
			"(merge/squash/rebase; default squash), commitMessage (default: PR\n"+
			"title + review rationale + referenced issues), resultFile (default\n"+
			"merge-result.json). Successful merges also report headBranch and\n"+
			"branchCleanup (deleted, skipped-stacked, or failed). Exit codes: 0 = evaluated\n"+
			"(merged or not — see the result file's \"merged\" field), 1 = business\n"+
			"error (missing capability/config, malformed inputs, provider failure),\n"+
			"2 = usage/IO error.\n")
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
	// The capability check IS the "capability absent → refused" acceptance
	// criterion: providerToken fails closed (no merge attempted, no PR
	// state even polled) unless this stage's declaration actually grants
	// github:pr:merge — the same fail-closed mechanism every other
	// provider-chain subcommand already relies on.
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
	verdict := providerInput("verdict", "")
	if verdict == "" {
		pf(stderr, "error: verdict input is required\n")
		return 1
	}
	if !apiv1.VerdictDecision(verdict).IsValid() {
		pf(stderr, "error: verdict input %q is not a known verdict decision\n", verdict)
		return 1
	}
	expectedHeadSHA := providerInput("headSha", "")
	if expectedHeadSHA == "" {
		pf(stderr, "error: headSha input is required (the SHA-pin, D6)\n")
		return 1
	}
	expectedBaseSHA := providerInput("baseSha", "")
	if expectedBaseSHA == "" {
		pf(stderr, "error: baseSha input is required (the SHA-pin, D6)\n")
		return 1
	}
	advisoryMode := providerInput("advisoryMode", "false") == "true"
	commitMessage := providerInput("commitMessage", "")
	mergeMethod := providers.MergeMethod(providerInput("mergeMethod", string(providers.MergeMethodSquash)))
	if !mergeMethod.IsValid() {
		pf(stderr, "error: mergeMethod input %q must be merge, squash, or rebase\n", mergeMethod)
		return 1
	}
	resultFile := providerInput("resultFile", "merge-result.json")

	ctx := context.Background()

	// #719: with merge-review's readiness allowing several concurrent runs
	// to review DIFFERENT PRs at once (distinct-PR concurrency is already
	// claim-ledger-safe, per pr-select), only ONE PR may be inside the
	// poll->decide->merge window at a time — an instance-wide flock, not a
	// distributed/network-backed lock (cheap, purely local). Without this,
	// two runs' polls could both observe the pre-merge base and both pass
	// their SHA-pin conjunct, even though the first run's merge (once it
	// lands) is exactly the kind of base movement #718's delta-aware check
	// exists to catch — the race only disappears if each run's poll is
	// guaranteed to see the truth AFTER any earlier run's merge already
	// completed, which serializing the whole window (not just the final
	// MergePullRequest call) guarantees. Branch cleanup after a successful
	// merge is independent per-PR state and does NOT need to be serialized.
	l := layoutFor(root)
	lockPath := filepath.Join(l.SchedulerDir(), mergeLockFileName)

	var poll providers.PullRequestPollResult
	var pollErr error
	var reasons []string
	var landResult mergepolicy.Result
	var mergeAttempted bool
	var mergeErr error
	var commitErr error
	var policyErr error
	lockErr := withClaimLock(lockPath, func() error {
		// Independent, live re-check (D6) — never trust a caller-supplied
		// "still valid" claim for CI/draft/SHA-pin; always re-poll the PR's
		// actual current state right before deciding, now guaranteed to be
		// the latest state relative to any other run's merge under this
		// same lock.
		poll, pollErr = provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
		if pollErr != nil {
			return nil
		}

		if apiv1.VerdictDecision(verdict) != apiv1.VerdictPass {
			reasons = append(reasons, fmt.Sprintf("verdict is %q, want pass", verdict))
		}
		if poll.CheckState != providers.CheckStatePassing {
			reasons = append(reasons, fmt.Sprintf("CI is %q, want passing", poll.CheckState))
		}
		if poll.Draft {
			reasons = append(reasons, "pull request is a draft")
		}
		if poll.HeadSHA != expectedHeadSHA {
			reasons = append(reasons, fmt.Sprintf("head moved: verdict pinned to %s, PR is now at %s — verdict is stale", expectedHeadSHA, poll.HeadSHA))
		}
		if poll.BaseSHA != expectedBaseSHA {
			// Delta-aware (issue #718): base moving at all used to void every
			// standing verdict, even when nothing that moved touches this PR
			// — the dominant false-invalidation case (any OTHER PR merging
			// advances base for everyone). Only a movement that actually
			// intersects this PR's own files still voids it.
			intersects, cerr := baseMovementIntersectsPR(ctx, provider, repo, pullNumber, expectedBaseSHA, poll.BaseSHA)
			switch {
			case cerr != nil:
				// Can't determine whether the movement is disjoint — fail
				// safe to the old conservative behavior rather than risk
				// merging past a base advance we couldn't actually check.
				reasons = append(reasons, fmt.Sprintf("base moved: verdict pinned to %s, PR is now based on %s, and whether that movement touches this PR's files could not be determined (%v) — treating as stale", expectedBaseSHA, poll.BaseSHA, cerr))
			case intersects:
				reasons = append(reasons, fmt.Sprintf("base moved: verdict pinned to %s, PR is now based on %s, and that movement touches files this PR also changes — verdict is stale", expectedBaseSHA, poll.BaseSHA))
			}
		}
		if advisoryMode {
			reasons = append(reasons, "advisory mode: no merge attempted")
		}
		if len(reasons) > 0 {
			return nil
		}

		// #528: the structured commit message is built from THIS locked
		// poll's verdict comment, not a separately (unlocked) re-fetched
		// one — #719's whole point is that everything from "decide to
		// merge" through the actual MergePullRequest call happens under
		// one lock, so a second, unlocked provider round-trip here would
		// silently reopen the exact race #719 closes. commitTitle stays
		// empty (provider default) whenever a caller-supplied commitMessage
		// is already set.
		commitTitle := ""
		mergeCommitMessage := commitMessage
		if strings.TrimSpace(mergeCommitMessage) == "" {
			commitTitle, mergeCommitMessage, commitErr = structuredMergeCommitMessage(poll)
			if commitErr != nil {
				return nil
			}
		}

		// Merge-policy detection (issue #758): direct-merge vs.
		// merge-queue-enqueue, detected per repo/branch from live branch
		// protection/ruleset state (cached — mergepolicycache.go — since
		// it's a live provider call). Resolved here, under the same lock,
		// using this poll's own BaseBranch — not a separately (unlocked)
		// re-fetched one — so the policy decision is made against exactly
		// the state this run's poll->decide->merge window already
		// serializes on, matching #528's structuredMergeCommitMessage
		// rationale just above.
		var policy providers.MergePolicy
		policy, policyErr = detectMergePolicy(ctx, provider, l.SchedulerDir(), repo, poll.BaseBranch, stderr)
		if policyErr != nil {
			return nil
		}
		lander, err := mergepolicy.ForPolicy(policy)
		if err != nil {
			policyErr = err
			return nil
		}

		mergeAttempted = true
		landResult, mergeErr = lander.Land(ctx, provider, mergepolicy.Request{
			Repository: repo, PullID: pullNumber, ExpectedHeadSHA: expectedHeadSHA,
			CommitTitle: commitTitle, CommitMessage: mergeCommitMessage, MergeMethod: mergeMethod,
		})
		return nil
	})
	if lockErr != nil {
		pf(stderr, "error: acquire merge lock: %v\n", lockErr)
		return 1
	}
	if pollErr != nil {
		return failProviderStage(stderr, "poll pull request", pollErr, "merge-result.json")
	}
	if len(reasons) > 0 {
		if err := writeMergeResult(resultFile, pullNumber, mergepolicy.Result{}, reasons, nil); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pf(stdout, "not merged (pr #%s): %s\n", pullNumber, strings.Join(reasons, "; "))
		return 0
	}
	if commitErr != nil {
		pf(stderr, "error: build merge commit message: %v\n", commitErr)
		return 1
	}
	if policyErr != nil {
		return failProviderStage(stderr, "detect merge policy", policyErr, "merge-result.json")
	}
	if mergeErr != nil {
		return failProviderStage(stderr, "merge pull request", mergeErr, "merge-result.json")
	}
	if !mergeAttempted {
		// Unreachable: either pollErr, reasons, commitErr, policyErr,
		// mergeErr, or a successful landing attempt always sets one of the
		// above.
		pf(stderr, "error: internal: merge-pr reached no decision for pr #%s\n", pullNumber)
		return 1
	}

	var cleanup *mergeBranchCleanup
	if landResult.Outcome == mergepolicy.OutcomeMerged {
		outcome := cleanupMergedBranch(ctx, poll.HeadRepository, poll.HeadBranch, provider)
		cleanup = &outcome
		if outcome.Error != "" {
			pf(stderr, "warning: merged pr #%s but branch cleanup failed: %s\n", pullNumber, outcome.Error)
		} else {
			pf(stdout, "branch cleanup %s (%s)\n", outcome.Status, outcome.HeadBranch)
		}
	}
	if err := writeMergeResult(resultFile, pullNumber, landResult, nil, cleanup); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if landResult.Outcome == mergepolicy.OutcomeEnqueued {
		pf(stdout, "enqueued pr #%s (merge queue)\n", pullNumber)
	} else {
		pf(stdout, "merged pr #%s (%s)\n", pullNumber, landResult.MergeSHA)
	}
	return 0
}

func structuredMergeCommitMessage(poll providers.PullRequestPollResult) (string, string, error) {
	title := strings.TrimSpace(poll.Title)
	if title == "" {
		return "", "", fmt.Errorf("pull request title is empty")
	}

	var verdict *apiv1.Verdict
	for i := len(poll.CommentsSince) - 1; i >= 0; i-- {
		candidate, ok := parseVerdictComment(poll.CommentsSince[i].Body)
		if !ok || candidate.Decision != apiv1.VerdictPass {
			continue
		}
		if candidate.HeadSHA != "" && candidate.HeadSHA != poll.HeadSHA {
			continue
		}
		if candidate.BaseSHA != "" && candidate.BaseSHA != poll.BaseSHA {
			continue
		}
		verdict = &candidate
		break
	}
	if verdict == nil {
		return "", "", fmt.Errorf("no current pass verdict with a summary or rationale found in pull request comments")
	}

	summary := strings.TrimSpace(verdict.Summary)
	rationale := strings.TrimSpace(verdict.Rationale)
	if summary == "" && rationale == "" {
		return "", "", fmt.Errorf("current pass verdict has no summary or rationale")
	}

	var parts []string
	if summary != "" {
		parts = append(parts, summary)
	}
	if rationale != "" && rationale != summary {
		parts = append(parts, rationale)
	}
	for _, issue := range closingIssueNumbers(poll.Body) {
		parts = append(parts, "Closes #"+issue)
	}
	return title, strings.Join(parts, "\n\n"), nil
}

type mergeBranchCleanup struct {
	Status     string
	HeadBranch string
	Error      string
}

func cleanupMergedBranch(ctx context.Context, headRepository *providers.RepositoryRef, headBranch string, prProvider *providers.GitHubProvider) mergeBranchCleanup {
	out := mergeBranchCleanup{HeadBranch: headBranch}
	recorder := sidecarMutationRecorder{kind: "branch"}
	fail := func(err error) mergeBranchCleanup {
		out.Status = "failed"
		out.Error = err.Error()
		return out
	}
	if headBranch == "" {
		return fail(fmt.Errorf("merged pull request did not report a head branch"))
	}
	if headRepository == nil {
		return fail(fmt.Errorf("merged pull request did not report a head repository"))
	}

	stacked, err := prProvider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     *headRepository,
		Base:           headBranch,
		SkipCheckState: true,
	})
	if err != nil {
		return fail(fmt.Errorf("check stacked pull requests for %q: %w", headBranch, err))
	}
	if len(stacked) > 0 {
		out.Status = "skipped-stacked"
		return out
	}

	token, err := providerToken(capability.GitHubBranchDelete)
	if err != nil {
		return fail(err)
	}
	branchProvider := newGitHubProvider(token, providers.WithMutationRecorder(recorder))
	if _, err := branchProvider.DeleteBranch(ctx, providers.DeleteBranchRequest{Repository: *headRepository, Name: headBranch}); err != nil {
		return fail(fmt.Errorf("delete branch %q: %w", headBranch, err))
	}
	out.Status = "deleted"
	return out
}

// baseMovementIntersectsPR reports whether base moving from oldBaseSHA to
// newBaseSHA touched any file pullNumber's own PR also changes (issue
// #718's delta-aware SHA-pin check): a disjoint base advance — the
// dominant steady-state case, since every OTHER PR merging moves base for
// everyone — must not void an otherwise-valid verdict, but a movement that
// genuinely intersects this PR's own files still must (a valid review
// against the old base says nothing about a file it never saw change).
func baseMovementIntersectsPR(ctx context.Context, provider providers.RepoProvider, repo providers.RepositoryRef, pullNumber, oldBaseSHA, newBaseSHA string) (bool, error) {
	prFiles, err := provider.PullRequestFiles(ctx, repo, pullNumber)
	if err != nil {
		return false, fmt.Errorf("list PR's own files: %w", err)
	}
	moved, err := provider.CompareCommits(ctx, repo, oldBaseSHA, newBaseSHA)
	if err != nil {
		return false, fmt.Errorf("compare base %s...%s: %w", oldBaseSHA, newBaseSHA, err)
	}
	prPaths := make(map[string]struct{}, len(prFiles))
	for _, f := range prFiles {
		prPaths[f.Path] = struct{}{}
	}
	for _, f := range moved.Files {
		if _, ok := prPaths[f.Path]; ok {
			return true, nil
		}
	}
	return false, nil
}

// writeMergeResult writes the declared result file's flat JSON —
// selectedNumber (string, always present), merged (bool, always present —
// true iff land.Outcome is mergepolicy.OutcomeMerged, i.e. GitHub reports
// this pull request actually merged; false for both the enqueued and
// refusal cases), landOutcome (string "merged"/"enqueued", present only when
// a landing was actually attempted — the #758 writeback distinct-state
// requirement: merge-gate's "land-outcome" check reads this, not merged, so
// enqueued is never conflated with merged), mergeSha (when merged),
// reason (a semicolon-joined list of unmet conjuncts, on refusal), and
// headBranch/branchCleanup/branchCleanupError (after an actual merge; a
// merely-enqueued pull request has nothing to clean up yet) — matching
// InputResultFile's flat-scalar-merge convention (internal/executor/shell.go's
// mergeResultFileOutputs). selectedNumber is echoed so the task after merge-gate
// can receive it through InputsFrom; merged is kept (unchanged meaning) so
// existing callers/tests reading only that boolean still see correct
// behavior for both the direct-merge and refusal cases.
func writeMergeResult(path, selectedNumber string, land mergepolicy.Result, reasons []string, cleanup *mergeBranchCleanup) error {
	out := map[string]interface{}{"selectedNumber": selectedNumber, "merged": land.Outcome == mergepolicy.OutcomeMerged}
	if land.Outcome != "" {
		out["landOutcome"] = string(land.Outcome)
	}
	if land.MergeSHA != "" {
		out["mergeSha"] = land.MergeSHA
	}
	if len(reasons) > 0 {
		out["reason"] = strings.Join(reasons, "; ")
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
		return fmt.Errorf("marshal merge result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
