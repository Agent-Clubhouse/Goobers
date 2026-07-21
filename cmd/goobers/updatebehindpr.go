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

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

type updateBehindAction uint8

const (
	updateBehindRouteFull updateBehindAction = iota
	updateBehindViaAPI
	updateBehindClearLabel
)

// runUpdateBehindPR is pr-remediation's API-only preflight. It terminates the
// workflow after updating a mechanically stale PR, or routes every non-trivial
// candidate into the existing worktree-backed gather/rebase/agentic path.
func runUpdateBehindPR(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update-behind-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers update-behind-pr [path]\n\n"+
			"Update one behind-base PR through GitHub's update-branch API when it\n"+
			"is mergeable, CI-clean, and carries no substantive findings. Other\n"+
			"candidates are routed to full remediation. Exit codes: 0 = updated,\n"+
			"routed, or no-work; 1 = business error; 2 = usage/IO error.\n")
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
	provider := newGitHubProvider(prToken)
	issuesProvider := newGitHubProvider(issuesToken)

	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo,
		Base:       providerInput("base", "main"),
		HeadPrefix: providerInput("headPrefix", providerBranchNamespace()),
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "update-behind-result.json")
	}
	prs, err = filterRemediationPullRequests(ctx, provider, repo, prs, nil)
	if err != nil {
		return failProviderStage(stderr, "filter remediation candidates", err, "update-behind-result.json")
	}

	baseTips := map[string]string{}
	behindByPR := map[int]bool{}
	behindBase := func(pr providers.PullRequestSummary) (bool, error) {
		behind, err := pullRequestBehindLiveBase(ctx, provider, repo, pr, baseTips)
		if err == nil {
			behindByPR[pr.Number] = behind
		}
		return behind, err
	}
	candidates, _, err := selectRemediationCandidates(prs, behindBase)
	if err != nil {
		return failProviderStage(stderr, "determine remediation eligibility", err, "update-behind-result.json")
	}
	if len(candidates) == 0 {
		return writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	claimed, err := claimEligiblePullRequest(root, candidates)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	candidate := *claimed
	action, err := updateBehindActionForPR(ctx, provider, repo, candidate, baseTips, behindByPR)
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

func updateBehindActionForPR(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string, behindByPR map[int]bool) (updateBehindAction, error) {
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
	if verdictHasSubstantiveFindingForPR(gatherPRVerdict(comments, verdictAuthor), pr.Number) {
		return updateBehindRouteFull, nil
	}
	if behind {
		return updateBehindViaAPI, nil
	}
	return updateBehindClearLabel, nil
}

func pullRequestBehindLiveBase(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (bool, error) {
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
