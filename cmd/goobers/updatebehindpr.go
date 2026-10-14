package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

type updateBehindAction uint8

const (
	updateBehindRouteFull updateBehindAction = iota
	updateBehindViaAPI
	updateBehindClearLabel
)

const updateBehindPRHelp = "Usage: goobers update-behind-pr [path]\n\n" +
	"Update one behind-base PR through GitHub's update-branch API when it\n" +
	"is mergeable, CI-clean, and carries no substantive findings. A behind\n" +
	"PR whose failing checks all failed on its base branch too, at its\n" +
	"merge-base or at a base commit since, is red because its base was, so\n" +
	"it is updated the same way. Other candidates are routed to full\n" +
	"remediation. A run dispatched for one\n" +
	"pull request (goobers run --pr, or a pull_request webhook delivery)\n" +
	"selects that PR and no other; when the target is not selectable the\n" +
	"stage reports no-work naming the reason instead of falling back to\n" +
	"another PR. On Azure DevOps this API-only lane is not applicable (ADO\n" +
	"has no up-to-date policy to satisfy via API update): the stage makes\n" +
	"no provider call and always routes to full remediation, which\n" +
	"reselects a candidate itself. Exit codes: 0 = updated, routed,\n" +
	"not-applicable, or no-work; 1 = business error; 2 = usage/IO error.\n"

// runUpdateBehindPR is pr-remediation's API-only preflight. It terminates the
// workflow after updating a mechanically stale PR, or routes every non-trivial
// candidate into the existing worktree-backed gather/rebase/agentic path.
func runUpdateBehindPR(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("update-behind-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "update-behind-pr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// ADO-N15 (docs/design/ado-parity-dsl-2-0.md §3.4): this stage's whole
	// premise — a cheap API-only update-branch call ahead of full
	// remediation — has no ADO analog, so it is reported not-applicable
	// before any provider token or client is constructed, exactly as the
	// derived-capabilities override for this stage declares
	// (stageProviderCapabilityOverrides in
	// internal/instance/providercapability.go: no pr.update-branch, no
	// pr.compare needed on ADO for this stage).
	if repo.Provider == providers.ProviderADO {
		return writeUpdateBehindNotApplicable(stdout, stderr, repo.Provider)
	}
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issuesToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, err := remediationStageProvider(root, repo, prToken, true)
	if err != nil {
		pf(stderr, "error: construct remediation provider: %v\n", err)
		return 1
	}
	issuesProvider, err := remediationStageProvider(root, repo, issuesToken, false)
	if err != nil {
		pf(stderr, "error: construct remediation issues provider: %v\n", err)
		return 1
	}

	// #3985: the pull request this run was dispatched for, when `goobers run
	// --pr` (or a pull_request webhook delivery) named one. Resolved before
	// any selection so every narrowing step below can be attributed in the
	// refusal an unselectable target produces.
	target := remediationTargetFromEnv()
	base := providerInput("base", providerBaseBranch())
	headPrefix := providerInput("headPrefix", providerBranchNamespace())

	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := remediationPullRequests(ctx, provider, repo, base, headPrefix, target)
	if err != nil {
		return failProviderStage(stderr, "select pull requests", err, "update-behind-result.json")
	}
	listed := prs
	prs, blockedDependents, err := filterRemediationPullRequests(ctx, provider, repo, prs, nil)
	if err != nil {
		return failProviderStage(stderr, "filter remediation candidates", err, "update-behind-result.json")
	}
	filtered := prs
	prs, err = stageClaimAvailablePullRequests(
		root, repo, os.Getenv(executor.RunIDEnvVar), prs, time.Now(),
	)
	if err != nil {
		return failProviderStage(stderr, "filter claimed remediation candidates", err, "update-behind-result.json")
	}
	unclaimed := prs

	baseTips := map[string]string{}
	behindByPR := map[int]bool{}
	behindBase := func(pr providers.PullRequestSummary) (bool, error) {
		behind, err := pullRequestBehindLiveBase(ctx, provider, repo, pr, baseTips)
		if err == nil {
			behindByPR[pr.Number] = behind
		}
		return behind, err
	}
	if err := resolveRemediationCheckStates(ctx, provider, repo, prs); err != nil {
		return failProviderStage(stderr, "resolve remediation check states", err, "update-behind-result.json")
	}
	candidates, _, err := selectRemediationCandidates(prs, blockedDependents, behindBase)
	if err != nil {
		return failProviderStage(stderr, "determine remediation eligibility", err, "update-behind-result.json")
	}
	// #3985: policy ranked the whole eligible set exactly as it does on a
	// scheduled tick; a targeted run then keeps only the PR the trigger named.
	// A target that ranking, claiming, or eligibility dropped ends the run with
	// the reason — never a fallback to the lane's next-best candidate.
	candidates, refusal := target.apply(
		remediationTargetStage{prs: listed, reason: remediationTargetUnlistedReason(base, headPrefix)},
		remediationTargetStage{prs: filtered, reason: remediationTargetFilteredReason},
		remediationTargetStage{prs: unclaimed, reason: remediationTargetClaimedReason},
		remediationTargetStage{prs: candidates, reason: remediationTargetIneligibleReason},
	)
	if refusal != "" {
		return writeNoWorkResult(stdout, stderr, refusal)
	}
	if len(candidates) == 0 {
		return writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	claimed, err := claimEligiblePullRequestInOrder(root, repo, candidates)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	candidate := *claimed
	minSeverity := resolveMinSeverity(stderr)
	action, err := updateBehindActionForPR(ctx, root, provider, repo, candidate, baseTips, behindByPR, minSeverity)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("check PR #%d for API branch update", candidate.Number), err, "update-behind-result.json")
	}
	if action == updateBehindRouteFull {
		return writeUpdateBehindResult(stdout, stderr, candidate.Number, true, false)
	}

	if action == updateBehindViaAPI {
		if _, err := provider.UpdateBranch(ctx, providers.UpdateBranchRequest{
			Repository:      repo,
			PullID:          strconv.Itoa(candidate.Number),
			ExpectedHeadSHA: candidate.HeadSHA,
		}); err != nil {
			var updateErr *providers.UpdateBranchError
			if errors.As(err, &updateErr) && updateErr.StatusCode == 422 {
				return writeUpdateBehindResult(stdout, stderr, candidate.Number, true, false)
			}
			return failProviderStage(stderr, fmt.Sprintf("update PR #%d branch", candidate.Number), err, "update-behind-result.json")
		}
	}
	if hasAnyLabel(candidate.Labels, []string{needsRemediationLabel}) {
		if _, err := issuesProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo,
			ID:         strconv.Itoa(candidate.Number),
			RemoveLabels: []string{
				needsRemediationLabel,
			},
		}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("clear %s from PR #%d", needsRemediationLabel, candidate.Number), err, "update-behind-result.json")
		}
	}
	return writeUpdateBehindResult(stdout, stderr, candidate.Number, false, action == updateBehindViaAPI)
}

func updateBehindActionForPR(ctx context.Context, root string, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string, behindByPR map[int]bool, minSeverity apiv1.Severity) (updateBehindAction, error) {
	if pr.CheckState == providers.CheckStateFailing {
		inherited, err := failingChecksInheritedFromBase(ctx, provider, repo, pr, baseTips)
		if err != nil || !inherited {
			return updateBehindRouteFull, err
		}
		behindByPR[pr.Number] = true
	}
	behind, known := behindByPR[pr.Number]
	if !known {
		var err error
		behind, err = pullRequestBehindLiveBase(ctx, provider, repo, pr, baseTips)
		if err != nil {
			return updateBehindRouteFull, err
		}
	}
	if !behind && !hasAnyLabel(pr.Labels, []string{needsRemediationLabel}) {
		return updateBehindRouteFull, nil
	}
	mergeable, err := provider.PullRequestMergeable(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return updateBehindRouteFull, err
	}
	if mergeable != nil && !*mergeable {
		return updateBehindRouteFull, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return updateBehindRouteFull, err
	}
	verdictAuthor, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return updateBehindRouteFull, err
	}
	if verdictHasSubstantiveFindingForPR(gatherPRVerdict(root, repo, pr.Number, comments, verdictAuthor), pr.Number, minSeverity) {
		return updateBehindRouteFull, nil
	}
	if behind {
		return updateBehindViaAPI, nil
	}
	return updateBehindClearLabel, nil
}

// Bounds on the base history failingChecksInheritedFromBase reads: how many
// first-parent base commits after the merge-base it checks, and how many
// failing ones it reads check names for.
const (
	inheritedBaseCommitWindow = 100
	inheritedBaseFailureReads = 10
)

// failingChecksInheritedFromBase reports whether a failing PR is red only
// because its base was (#7071): the PR is behind its live base and every
// check failing on its head also failed on the base, at the PR's merge-base
// or at a base commit between the merge-base and the live tip. Such a PR
// needs its branch updated and CI re-run, not an agentic remediation whose
// agent finds nothing in the PR's diff to change and then parks it as
// no-work. Pull-request CI runs against the base tip of its trigger time, so
// a base commit after the merge-base can be the one that turned it red.
//
// A failing check the base never failed in that range is the PR's own, so it
// routes the PR to full remediation. So does a failure the provider does not
// name, and so does base history the provider does not report (history
// beyond the one listed page is not read): nothing then shows the base
// explains the failure. A base that merely passes now is not evidence; on a
// busy base it would update a genuinely broken PR on every base move. Once
// updated, the merge-base is the tip the update merged, so the PR is updated
// again on this rule only if the base fails the same checks after that.
func failingChecksInheritedFromBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (bool, error) {
	mergeBase, baseTip, err := pullRequestLiveBaseMergeBase(ctx, provider, repo, pr, baseTips)
	if err != nil || mergeBase == baseTip {
		return false, err
	}
	headFailures, err := provider.CIFailures(ctx, repo, pr.HeadSHA)
	if err != nil {
		return false, fmt.Errorf("list failing checks on PR #%d head: %w", pr.Number, err)
	}
	unexplained := make(map[string]bool, len(headFailures))
	for _, failure := range headFailures {
		if failure.Name == "" {
			return false, nil
		}
		unexplained[failure.Name] = true
	}
	if len(unexplained) == 0 {
		return false, nil
	}
	refs, err := baseCommitsFromMergeBase(ctx, provider, repo, mergeBase, baseTip)
	if err != nil {
		return false, fmt.Errorf("list PR #%d base commits since merge-base %s: %w", pr.Number, mergeBase, err)
	}
	return baseFailuresExplain(ctx, provider, repo, refs, unexplained)
}

// firstParentHistoryReader is the base-history read
// failingChecksInheritedFromBase uses. The GitHub and Gitea providers both
// implement it; a provider that does not leaves only the merge-base as
// evidence.
type firstParentHistoryReader interface {
	FirstParentHistory(ctx context.Context, repo providers.RepositoryRef, tip, stop string, limit int) ([]string, error)
}

var (
	_ firstParentHistoryReader = (*providers.GitHubProvider)(nil)
	_ firstParentHistoryReader = (*providers.GiteaProvider)(nil)
)

// baseCommitsFromMergeBase returns mergeBase followed by the base branch's
// own first-parent commits after it, newest first, when that chain reaches
// mergeBase within inheritedBaseCommitWindow commits; otherwise mergeBase
// alone. Commits of branches merged into the base are left out: their checks
// ran as pull-request CI, never on the base.
func baseCommitsFromMergeBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, mergeBase, baseTip string) ([]string, error) {
	reader, ok := provider.(firstParentHistoryReader)
	if !ok {
		return []string{mergeBase}, nil
	}
	history, err := reader.FirstParentHistory(ctx, repo, baseTip, mergeBase, inheritedBaseCommitWindow)
	if err != nil {
		return nil, err
	}
	return append([]string{mergeBase}, history...), nil
}

// baseFailuresExplain reports whether the base commits in refs whose checks
// are failing, between them, fail every check named in unexplained. It reads
// check names for at most inheritedBaseFailureReads failing commits.
func baseFailuresExplain(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, refs []string, unexplained map[string]bool) (bool, error) {
	states, err := provider.RefCheckStates(ctx, repo, refs)
	if err != nil {
		return false, fmt.Errorf("resolve base commit check states: %w", err)
	}
	reads := 0
	for _, ref := range refs {
		if states[ref] != providers.CheckStateFailing {
			continue
		}
		if reads == inheritedBaseFailureReads {
			return false, nil
		}
		reads++
		failures, err := provider.CIFailures(ctx, repo, ref)
		if err != nil {
			return false, fmt.Errorf("list failing checks on base commit %s: %w", ref, err)
		}
		for _, failure := range failures {
			delete(unexplained, failure.Name)
		}
		if len(unexplained) == 0 {
			return true, nil
		}
	}
	return false, nil
}
func pullRequestBehindLiveBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (bool, error) {
	mergeBase, baseTip, err := pullRequestLiveBaseMergeBase(ctx, provider, repo, pr, baseTips)
	if err != nil {
		return false, err
	}
	return mergeBase != baseTip, nil
}

// pullRequestLiveBaseMergeBase resolves pr's merge-base with the live tip of
// its base branch, caching the tip per base in baseTips.
func pullRequestLiveBaseMergeBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (mergeBase, baseTip string, err error) {
	baseTip = baseTips[pr.Base]
	if baseTip == "" {
		baseTip, err = provider.BranchTipSHA(ctx, repo, pr.Base)
		if err != nil {
			return "", "", fmt.Errorf("resolve live base branch %q: %w", pr.Base, err)
		}
		baseTips[pr.Base] = baseTip
	}
	compared, err := provider.CompareCommits(ctx, repo, baseTip, pr.HeadSHA)
	if err != nil {
		return "", "", fmt.Errorf("compare live base with PR #%d head: %w", pr.Number, err)
	}
	if compared.MergeBaseSHA == "" {
		return "", "", fmt.Errorf("compare live base with PR #%d head returned no merge base", pr.Number)
	}
	return compared.MergeBaseSHA, baseTip, nil
}

// writeUpdateBehindNotApplicable reports update-behind-pr as not applicable
// to provider (ADO-N15, docs/design/ado-parity-dsl-2-0.md §3.4) without
// selecting or claiming any candidate. selectedNumber is left "" rather than
// "0": an empty handoff reads as no pinned candidate
// (gatherPRContextCandidateScope, gatherprcontext.go), so gather-pr-context
// performs its own fresh selection instead of failing on an out-of-range PR
// number. needsFullRemediation stays "true" so update-behind-gate routes into
// gather-pr-context and the remediation chain continues — emitting no-work
// here would end every ADO pr-remediation run instead.
func writeUpdateBehindNotApplicable(stdout, stderr io.Writer, provider providers.ProviderKind) int {
	resultFile := providerInput("resultFile", "update-behind-result.json")
	reason := fmt.Sprintf("update-behind-pr is not applicable on %s: routing to full remediation", provider)
	data, err := json.Marshal(map[string]string{
		"selectedNumber":       "",
		"needsFullRemediation": "true",
		"notApplicable":        "true",
		"reason":               reason,
	})
	if err != nil {
		pf(stderr, "error: marshal update-behind result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "%s\n", reason)
	return 0
}

func writeUpdateBehindResult(stdout, stderr io.Writer, selectedNumber int, needsFullRemediation, updated bool) int {
	resultFile := providerInput("resultFile", "update-behind-result.json")
	data, err := json.Marshal(map[string]string{
		"selectedNumber":       strconv.Itoa(selectedNumber),
		"needsFullRemediation": strconv.FormatBool(needsFullRemediation),
	})
	if err != nil {
		pf(stderr, "error: marshal update-behind result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	if needsFullRemediation {
		pf(stdout, "PR #%d requires full remediation\n", selectedNumber)
	} else if updated {
		pf(stdout, "PR #%d: updated behind branch through GitHub API and cleared %s when present\n", selectedNumber, needsRemediationLabel)
	} else {
		pf(stdout, "PR #%d: branch is current; cleared retained %s\n", selectedNumber, needsRemediationLabel)
	}
	return 0
}
