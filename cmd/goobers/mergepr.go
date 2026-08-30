package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/providers"
)

// runMergePR implements the `goobers merge-pr` built-in stage kind (issue
// #360): the provider-level conjunctive auto-merge action `merge-review`
// drives. It merges a PR only when EVERY independent conjunct holds —
// verdict=pass, CI green, not a draft, the SHA-pin (headSha/baseSha) still
// matches the PR's LIVE state, and — for a sibling-overlap PR — completed
// single-lander election evidence (#1071) — never a bare self-approval, and
// never trusting a caller-supplied "still valid" claim instead of re-polling
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
const (
	// These stable refusal tokens are threaded through merge-review's fail
	// branch and recorded against the unchanged pull request head.
	mergeConflictReason         = "merge-conflict"
	requiredStatusPendingReason = "required-status-check-pending"
)

const (
	mergeReviewOptOutOutcome = "skipped"
	mergeReviewOptOutReason  = "pull request is labeled " + noMergeReviewLabel
	// runAbortedOptOutReason is the refusal merge-pr records for a PR carrying
	// abortedRunLabel (#2238): the originating implementation run was
	// cancelled, so this PR must not auto-merge even if pr-select somehow
	// still selected it (a stale selection, a targeted re-trigger bypassing
	// pr-select) and even with a green verdict and passing CI — defense in
	// depth alongside pr-select's own exclusion of the label.
	runAbortedOptOutReason = "pull request is labeled " + abortedRunLabel
)

const mergePRHelp = "Usage: goobers merge-pr [path]\n\n" +
	"Merge a pull request, but only when every independent conjunct holds:\n" +
	"verdict=pass, CI green, not a draft, the SHA-pin still matches the PR's\n" +
	"live head/base, and — for a sibling-overlap PR — completed single-lander\n" +
	"election evidence (elected:true, #1071) — never a bare self-approval.\n" +
	"Declared inputs: pullNumber, verdict, headSha, baseSha (all required),\n" +
	"verdictAuthor (required for the default commit message; supplied by\n" +
	"apply-verdict), advisoryMode (default false — report only, no merge\n" +
	"attempted), mergeMethod (merge/squash/rebase; default squash),\n" +
	"commitMessage (default: PR title + review rationale + referenced\n" +
	"issues), resultFile (default merge-result.json). Successful merges\n" +
	"also report headBranch and branchCleanup (deleted, skipped-stacked, or\n" +
	"failed). Exit codes: 0 = evaluated (merged or not — see the result\n" +
	"file's \"merged\" field), 1 = business error (missing capability/config,\n" +
	"malformed inputs, provider failure), 2 = usage/IO error.\n"

// ciReadyForMerge reports whether the PR's CI permits a merge. A non-passing
// aggregate CheckState normally blocks — but the check-state this codebase
// derives from raw check-runs cannot tell a required check from an advisory
// (continue-on-error) one, so a single red advisory check would wrongly gate an
// otherwise-mergeable PR (#961). GitHub answers that distinction authoritatively
// via mergeable_state: "unstable" means the PR IS mergeable and every red or
// pending check is NON-required. So when the provider reports that state, CI is
// merge-ready despite a red advisory check. Any other state (including a
// provider that supplies none — empty MergeableState, e.g. ADO) falls through
// to the conservative "all checks must pass" gate, so a required-check failure,
// a genuinely-blocked PR, or an unknown state still blocks exactly as before.
func ciReadyForMerge(poll providers.PullRequestPollResult) bool {
	if poll.CheckState == providers.CheckStatePassing {
		return true
	}
	return poll.MergeableState == providers.MergeableStateUnstable
}

func runMergePR(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("merge-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "merge-pr")
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
	// Azure DevOps merge/completion authority rides on its dedicated capability;
	// provider construction still goes through the shared stage factory. The
	// GitHub-only helpers below (tutor classification, branch cleanup) stay
	// unreachable while every conjunct check between here and the land is shared.
	isADO := repo.Provider == providers.ProviderADO
	isGitHub := repo.Provider == providers.ProviderGitHub

	// prProvider is the provider-neutral forge surface the non-ADO helpers
	// require. dispatcher is the provider-neutral landing seam (CONF-1 #2074)
	// every poll/compare/detect/enqueue/merge call flows through, so every
	// registered provider runs one shared code path.
	providerCapability := capability.GitHubPRMerge
	if isADO {
		// Merge/completion authority on ADO rides on the dedicated
		// capability.ADOPRComplete ("ado:pr:complete") — the ADO counterpart to
		// github:pr:merge — so the decider≠executor capability isolation
		// (docs/design/v0/pr-lifecycle-loop.md §7) is preserved on ADO too.
		// Resolve that grant fail-closed FIRST (mirroring the github:pr:merge
		// check on the GitHub branch), then construct the completion-authorized
		// provider: a stage carrying only ado:pr:write must never silently
		// acquire completion authority (merge-wiring-plan §3).
		if _, err := providerToken(capability.ADOPRComplete); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		providerCapability = capability.ADOPRComplete
	}
	stageProvider, err := newMergeReviewProvider(root, repo, false,
		withStageProviderCapability(providerCapability),
		withStageProviderCache(),
		withStageProviderMutations("pr"),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	dispatcher := providers.NewDispatcher(stageProvider)
	var prProvider mergeProvider
	if !isADO {
		var ok bool
		prProvider, ok = stageProvider.(mergeProvider)
		if !ok {
			pf(stderr, "error: repository provider %q does not support merge lifecycle helpers\n", repo.Provider)
			return 1
		}
	}

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
	verdictAuthor := strings.TrimSpace(providerInput("verdictAuthor", ""))
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

	ctx, cancel := providerCommandContext()
	defer cancel()

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
	//
	// Over the claims plane (a stage pod, which has no instance flock) the
	// same window is a lease on the synthetic item merge-lock/<owner>/<repo>
	// held by this run: acquire polls until held, the lease is renewed while
	// the window runs, and a crashed holder's lease lapses on its own instead
	// of leaking a flock (finding 002 C1). Same-host stages keep the flock.
	l := layoutFor(root)
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		pf(stderr, "error: open claim ledger: %v\n", err)
		return 1
	}
	mergeLock := claimsclient.MergeLock{
		Key:      claimsclient.MergeLockKey(providerGaggle(), string(repo.Provider), repo.Owner, repo.Name),
		RunID:    os.Getenv(executor.RunIDEnvVar),
		Workflow: os.Getenv(executor.WorkflowEnvVar),
	}

	var poll providers.PullRequestPollResult
	var pollErr error
	var reasons []string
	var landResult mergepolicy.Result
	var mergeAttempted bool
	var mergeErr error
	var commitErr error
	var policyErr error
	var optedOutReason string
	lockErr := ledger.MergeLock(ctx, mergeLock, func(ctx context.Context) error {
		// Independent, live re-check (D6) — never trust a caller-supplied
		// "still valid" claim for CI/draft/SHA-pin; always re-poll the PR's
		// actual current state right before deciding, now guaranteed to be
		// the latest state relative to any other run's merge under this
		// same lock.
		// PollPullRequest is a mandatory Provider method: routing it through the
		// dispatcher (which promotes the embedded Provider's method unchanged)
		// works for both GitHub and ADO — behavior on GitHub is identical to the
		// former concrete call.
		poll, pollErr = dispatcher.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
		if pollErr != nil {
			return nil
		}
		if hasAnyLabel(poll.Labels, []string{noMergeReviewLabel}) {
			optedOutReason = mergeReviewOptOutReason
			return nil
		}
		// #2238: independently refuse a PR whose originating implementation
		// run was cancelled, even if pr-select's own exclusion of this same
		// label was somehow bypassed — the final merge primitive must not
		// rely solely on selection-time filtering.
		if hasAnyLabel(poll.Labels, []string{abortedRunLabel}) {
			optedOutReason = runAbortedOptOutReason
			return nil
		}

		if apiv1.VerdictDecision(verdict) != apiv1.VerdictPass {
			reasons = append(reasons, fmt.Sprintf("verdict is %q, want pass", verdict))
		}
		if !ciReadyForMerge(poll) {
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
			intersects, cerr := baseMovementIntersectsPR(ctx, dispatcher, repo, pullNumber, expectedBaseSHA, poll.BaseSHA)
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
		// Tutor-change classification is GitHub-only: classifyRemoteTutorChanges
		// takes the concrete *GitHubProvider and the tutor lane does not run on
		// ADO, so gate the whole block to GitHub (no-op; ADO tutor parity is
		// tracked by the ADO merge epic, merge-wiring-plan §1a).
		if isGitHub && isTutorBranch(poll.HeadBranch, providerBranchNamespace()) {
			classification, classifyErr := classifyRemoteTutorChanges(
				ctx, prProvider, repo, pullNumber, poll.BaseSHA, poll.HeadSHA,
			)
			switch {
			case classifyErr != nil:
				reasons = append(reasons, fmt.Sprintf("Tutor change classification failed (%v) — explicit human review required", classifyErr))
			case classification.RequiresHumanSignoff():
				reasons = append(reasons, tutorManualReviewReason(classification))
			}
		}
		// #1071: GitHub's native merge queue must never be a second, self-
		// arbitrating merge authority for a sibling-overlap PR. The canonical
		// pinned pass verdict (the same trusted comment structuredMergeCommitMessage
		// reads below) is the durable, SHA-keyed evidence of whether single-lander
		// election actually crowned this PR; refuse to land whenever it's an
		// overlap-cluster member apply-verdict has NOT (yet, or no longer) elected —
		// a normal, fail-closed refusal, never a caller-supplied claim.
		if pinned, ok := pinnedPassVerdict(poll, verdictAuthor); ok && pinned.OverlapCluster && !pinned.Elected {
			reasons = append(reasons, "sibling-overlap PR has no completed election evidence (elected:true) for the current head/base — refusing native-queue landing (#1071)")
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
			switch {
			case isADO:
				// THE single hard blocker (merge-wiring-plan §1a/§2/§8): ADO
				// PollPullRequest returns an empty CommentsSince (there is no
				// merge-review sticky-verdict comment surface on ADO), so
				// structuredMergeCommitMessage's pinnedPassVerdict lookup ALWAYS
				// fails on ADO — a clean pass with a green Build policy would set
				// commitErr and hard-return 1 below, never landing. Build the
				// commit directly from the PR's own title + closing refs (the same
				// non-verdict assembly structuredMergeCommitMessage does at
				// mergepr.go:443-453), bypassing the verdict comment. verdictAuthor
				// is not required on ADO (no comment to attribute).
				commitTitle, mergeCommitMessage, commitErr = adoMergeCommitMessage(poll)
				if commitErr != nil {
					return nil
				}
			case verdictAuthor == "":
				commitErr = fmt.Errorf("verdictAuthor input is required when commitMessage is empty")
				return nil
			default:
				commitTitle, mergeCommitMessage, commitErr = structuredMergeCommitMessage(poll, verdictAuthor)
				if commitErr != nil {
					return nil
				}
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
		policy, policyErr = detectMergePolicy(ctx, dispatcher, l.SchedulerDir(), repo, poll.BaseBranch, stderr)
		if policyErr != nil {
			return nil
		}
		lander, err := mergepolicy.ForPolicy(policy)
		if err != nil {
			policyErr = err
			return nil
		}

		mergeAttempted = true
		landResult, mergeErr = lander.Land(ctx, dispatcher, mergepolicy.Request{
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
	if optedOutReason != "" {
		if err := writeSkippedMergeResult(resultFile, pullNumber, expectedHeadSHA, optedOutReason); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pf(stdout, "skipped pr #%s: %s\n", pullNumber, optedOutReason)
		return 0
	}
	if len(reasons) > 0 {
		if err := writeMergeResult(resultFile, pullNumber, expectedHeadSHA, mergepolicy.Result{}, reasons, nil); err != nil {
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
		// Confirmed merge conflicts and pending required checks are business
		// refusals, not provider failures. Emit the standard refusal envelope
		// so merge-review can record and route them; unrecognized provider
		// errors retain the generic failure behavior.
		reason := ""
		switch {
		case providers.IsMergeConflictError(mergeErr):
			reason = mergeConflictReason
		case providers.IsRequiredStatusCheckPendingError(mergeErr):
			reason = requiredStatusPendingReason
		}
		if reason != "" {
			if err := writeMergeResult(resultFile, pullNumber, expectedHeadSHA, mergepolicy.Result{}, []string{reason}, nil); err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			pf(stdout, "not merged (pr #%s): %s\n", pullNumber, reason)
			return 0
		}
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
	// Branch cleanup is unavailable on ADO: its PollPullRequest does not populate
	// HeadRepository, so it could only fail "did not report a head repository".
	// Gate OFF
	// (no-op); ADO source-branch deletion rides on the enqueue/merge
	// deleteSourceBranch flag, out of scope for this epic (merge-wiring-plan
	// §1a/§8). GitHub and Gitea use the shared provider-neutral cleanup path.
	if !isADO && landResult.Outcome == mergepolicy.OutcomeMerged {
		outcome := cleanupMergedBranch(ctx, root, poll.HeadRepository, poll.HeadBranch, prProvider)
		cleanup = &outcome
		if outcome.Error != "" {
			pf(stderr, "warning: merged pr #%s but branch cleanup failed: %s\n", pullNumber, outcome.Error)
		} else {
			pf(stdout, "branch cleanup %s (%s)\n", outcome.Status, outcome.HeadBranch)
		}
	}
	if err := writeMergeResult(resultFile, pullNumber, expectedHeadSHA, landResult, nil, cleanup); err != nil {
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

// pinnedPassVerdict finds the trusted merge-review sticky comment carrying a
// pass verdict pinned to poll's LIVE head/base SHA — the same authoritative,
// re-pollable source structuredMergeCommitMessage builds its commit message
// from and the sibling-overlap election conjunct (#1071) checks before
// landing. Only the first trusted merge-review status comment is ever
// inspected (mirroring reconcileMergeReviewStatusCommentAs's single-canonical-
// comment invariant); a parse failure or a pin mismatch is "not found", never
// an error — an unusual but normal outcome the caller reports as a refusal.
func pinnedPassVerdict(poll providers.PullRequestPollResult, verdictAuthor string) (apiv1.Verdict, bool) {
	for _, comment := range poll.CommentsSince {
		if !isTrustedMergeReviewStatusComment(comment.Author, comment.Body, verdictAuthor) {
			continue
		}
		candidate, ok := parseVerdictComment(comment.Body)
		if !ok {
			break
		}
		if candidate.Decision == apiv1.VerdictPass &&
			candidate.HeadSHA != "" && candidate.HeadSHA == poll.HeadSHA &&
			candidate.BaseSHA != "" && candidate.BaseSHA == poll.BaseSHA {
			return candidate, true
		}
		break
	}
	return apiv1.Verdict{}, false
}

func structuredMergeCommitMessage(poll providers.PullRequestPollResult, verdictAuthor string) (string, string, error) {
	title := strings.TrimSpace(poll.Title)
	if title == "" {
		return "", "", fmt.Errorf("pull request title is empty")
	}

	verdict, ok := pinnedPassVerdict(poll, verdictAuthor)
	if !ok {
		return "", "", fmt.Errorf("canonical merge-review status is not a pass verdict pinned to the current head and base")
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

// adoMergeCommitMessage builds the land's commit title and body for an Azure
// DevOps pull request directly from the PR's own fields, bypassing the
// merge-review sticky-comment verdict lookup structuredMergeCommitMessage
// depends on. ADO PollPullRequest returns an empty CommentsSince (ADO has no
// merge-review sticky-verdict comment surface — merge-wiring-plan §2), so the
// verdict-rationale assembly is unavailable there; the title and "Closes #N"
// closing refs are exactly the non-verdict parts the GitHub assembly already
// produces (mergepr.go:443-453). This is the fix for the single hard blocker:
// without it, structuredMergeCommitMessage always errors on ADO and a clean
// green-Build-policy pass hard-fails the stage instead of landing
// (merge-wiring-plan §1a/§8). Errors only on the same empty-title condition the
// GitHub assembly rejects, so an ADO PR with no title is still a business error.
func adoMergeCommitMessage(poll providers.PullRequestPollResult) (string, string, error) {
	title := strings.TrimSpace(poll.Title)
	if title == "" {
		return "", "", fmt.Errorf("pull request title is empty")
	}
	var parts []string
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

func cleanupMergedBranch(ctx context.Context, root string, headRepository *providers.RepositoryRef, headBranch string, prProvider mergeProvider) mergeBranchCleanup {
	out := mergeBranchCleanup{HeadBranch: headBranch}
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

	// Build the delete through a branch-scoped recorder so the journal records
	// kind="branch", distinct from the merge that preceded it. Provider dispatch
	// is retained for Gitea instead of falling back to api.github.com.
	branchStageProvider, err := newProviderForStage(root, *headRepository, false,
		withStageProviderCapability(capability.GitHubBranchDelete),
		withStageProviderMutations("branch"),
	)
	if err != nil {
		return fail(err)
	}
	branchProvider, ok := branchStageProvider.(providers.BranchDeleter)
	if !ok {
		return fail(fmt.Errorf("repository provider %q does not support branch deletion", headRepository.Provider))
	}
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
func baseMovementIntersectsPR(ctx context.Context, provider *providers.Dispatcher, repo providers.RepositoryRef, pullNumber, oldBaseSHA, newBaseSHA string) (bool, error) {
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
// this pull request actually merged; false for enqueued, skipped, and
// refusal cases), optedOut (bool, always present), landOutcome (string
// "merged"/"enqueued" when landing was attempted, or "skipped" for a
// terminal opt-out), mergeSha (when merged),
// reason (a semicolon-joined list of unmet conjuncts, on refusal), and
// headBranch/branchCleanup/branchCleanupError (after an actual merge; a
// merely-enqueued pull request has nothing to clean up yet) — matching
// InputResultFile's flat-scalar-merge convention (internal/executor/shell.go's
// mergeResultFileOutputs). selectedNumber is echoed so the task after merge-gate
// can receive it through InputsFrom; merged is kept (unchanged meaning) so
// existing callers/tests reading only that boolean still see correct
// behavior for both the direct-merge and refusal cases.
func writeMergeResult(path, selectedNumber, selectedHeadSha string, land mergepolicy.Result, reasons []string, cleanup *mergeBranchCleanup) error {
	return writeMergeResultFields(path, selectedNumber, selectedHeadSha, string(land.Outcome), land.MergeSHA, reasons, cleanup)
}

func writeSkippedMergeResult(path, selectedNumber, selectedHeadSha, reason string) error {
	return writeMergeResultFields(
		path, selectedNumber, selectedHeadSha, mergeReviewOptOutOutcome, "",
		[]string{reason}, nil,
	)
}

func writeMergeResultFields(path, selectedNumber, selectedHeadSha, landOutcome, mergeSHA string, reasons []string, cleanup *mergeBranchCleanup) error {
	out := map[string]interface{}{
		"selectedNumber": selectedNumber,
		"merged":         landOutcome == string(mergepolicy.OutcomeMerged),
		"optedOut":       landOutcome == mergeReviewOptOutOutcome,
	}
	// Echo the SHA-pin so record-merge-refusal (#950) can key a demotion by the
	// head the refusal happened at (threaded via inputsFrom on the fail branch);
	// a refusal at a new head is a fresh attempt, not a continuation.
	out["selectedHeadSha"] = selectedHeadSha
	if landOutcome != "" {
		out["landOutcome"] = landOutcome
	}
	if mergeSHA != "" {
		out["mergeSha"] = mergeSHA
	}
	// Always emit reason (empty on a successful merge/enqueue) so it is a
	// declarable output record-merge-refusal (#950) can thread via inputsFrom
	// on merge-gate's fail branch — the demotion recorder needs the refusal
	// text to exclude advisory-mode "refusals" and to explain the demotion.
	out["reason"] = strings.Join(reasons, "; ")
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
