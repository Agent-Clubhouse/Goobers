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
	"is mergeable, CI-clean, and carries no substantive findings. Other\n" +
	"candidates are routed to full remediation. A run dispatched for one\n" +
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
	// derived-capabilities override above declares (no pr.update-branch,
	// no pr.compare needed on ADO for this stage).
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
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     repo,
		Base:           base,
		HeadPrefix:     headPrefix,
		SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "update-behind-result.json")
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
		return updateBehindRouteFull, nil
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

func pullRequestBehindLiveBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (bool, error) {
	baseTip := baseTips[pr.Base]
	if baseTip == "" {
		var err error
		baseTip, err = provider.BranchTipSHA(ctx, repo, pr.Base)
		if err != nil {
			return false, fmt.Errorf("resolve live base branch %q: %w", pr.Base, err)
		}
		baseTips[pr.Base] = baseTip
	}
	compared, err := provider.CompareCommits(ctx, repo, baseTip, pr.HeadSHA)
	if err != nil {
		return false, fmt.Errorf("compare live base with PR #%d head: %w", pr.Number, err)
	}
	if compared.MergeBaseSHA == "" {
		return false, fmt.Errorf("compare live base with PR #%d head returned no merge base", pr.Number)
	}
	return compared.MergeBaseSHA != baseTip, nil
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
