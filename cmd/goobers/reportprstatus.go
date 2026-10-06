package main

import (
	"errors"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

const reportPRStatusHelp = "Usage: goobers report-pr-status [path]\n\n" +
	"Publish a provider-native pull-request status (Azure DevOps PR status) as\n" +
	"goobers' own evidence — the agentic reviewer verdict and the local-CI\n" +
	"result — so a repository's status-check branch policy can gate on it and\n" +
	"the validation loop can prove PR correctness against the repo's required\n" +
	"policies (#772). Reaching this stage on the happy path is itself the\n" +
	"evidence: the run only advances here after the review gate and local-CI\n" +
	"gate both pass, so the default published state is `succeeded`.\n\n" +
	"Inputs (Task.Inputs / inputsFrom): prNumber (required, from open-pr),\n" +
	"statusName (default \"validation\"), statusGenre (default \"goobers\"),\n" +
	"state (succeeded|failed|pending, default succeeded), description,\n" +
	"targetUrl (default the PR url), headSha (optional: the commit the evidence\n" +
	"covers; Gitea posts the status on it, Azure DevOps refuses when the PR\n" +
	"head has moved past it), resultFile (default status-result.json).\n" +
	"Exit codes: 0 = published, 1 = business error, 2 = usage/IO error.\n"

// reportPRStatusPublisher is the narrow surface report-pr-status needs. Only
// providers that publish a native, policy-gate-able PR status satisfy it
// (Azure DevOps today); a GitHub run reports an actionable error rather than
// silently succeeding.
type reportPRStatusPublisher interface {
	providers.PullRequestStatusPublisher
}

func runReportPRStatus(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("report-pr-status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "report-pr-status")
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

	prNumber := providerInput("prNumber", "")
	if prNumber == "" {
		pf(stderr, "error: prNumber is required (wire it from open-pr via inputsFrom)\n")
		return 1
	}

	if repo.Provider == providers.ProviderGitHub {
		pf(stderr, "error: provider %q does not support publishing a policy-gate-able PR status (Azure DevOps and Gitea only, #772)\n", repo.Provider)
		return 1
	}
	provider, err := newProviderForStage(root, repo, false, withStageProviderCapability(capability.GitHubPRWrite))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	publisher, ok := provider.(reportPRStatusPublisher)
	if !ok {
		pf(stderr, "error: provider %q does not support publishing a policy-gate-able PR status (Azure DevOps and Gitea only, #772)\n", repo.Provider)
		return 1
	}

	state, err := parseStatusState(providerInput("state", "succeeded"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	req := providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      prNumber,
		Genre:       providerInput("statusGenre", "goobers"),
		Name:        providerInput("statusName", "validation"),
		State:       state,
		Description: providerInput("description", "goobers validation passed (review + local CI)"),
		TargetURL:   providerInput("targetUrl", providerInput("pull-request-url", "")),
		HeadSHA:     providerInput("headSha", ""),
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	resultFile := providerInput("resultFile", "status-result.json")
	result, err := prstatus.Publish(ctx, publisher, req, resultFile)
	if err != nil {
		var writeErr *prstatus.ResultWriteError
		if errors.As(err, &writeErr) {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		return failProviderStage(stderr, "publish pull request status", err, "status-result.json")
	}

	pf(stdout, "published pr #%s status %s/%s = %s (id %d)\n", prNumber, req.Genre, req.Name, state, result.ID)
	return 0
}

// parseStatusState maps a workflow-declared status verb to the provider-neutral
// CheckState the status-publish primitive posts. Accepts both the ADO-native
// verbs (succeeded/failed/pending) and the internal CheckState names so a
// workflow author can use either.
func parseStatusState(v string) (providers.CheckState, error) {
	return prstatus.ParseState(v)
}
