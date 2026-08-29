package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
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
	"targetUrl (default the PR url), resultFile (default status-result.json).\n" +
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
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	result, err := publisher.PublishPullRequestStatus(ctx, req)
	if err != nil {
		return failProviderStage(stderr, "publish pull request status", err, "status-result.json")
	}

	resultFile := providerInput("resultFile", "status-result.json")
	if err := writeReportPRStatusResult(resultFile, result.ID, prNumber, req.Genre, req.Name, string(state)); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	pf(stdout, "published pr #%s status %s/%s = %s (id %d)\n", prNumber, req.Genre, req.Name, state, result.ID)
	return 0
}

// parseStatusState maps a workflow-declared status verb to the provider-neutral
// CheckState the status-publish primitive posts. Accepts both the ADO-native
// verbs (succeeded/failed/pending) and the internal CheckState names so a
// workflow author can use either.
func parseStatusState(v string) (providers.CheckState, error) {
	switch v {
	case "succeeded", "success", "passing":
		return providers.CheckStatePassing, nil
	case "failed", "failure", "failing":
		return providers.CheckStateFailing, nil
	case "pending", "":
		return providers.CheckStatePending, nil
	default:
		return "", fmt.Errorf("unknown status state %q (want succeeded|failed|pending)", v)
	}
}

func writeReportPRStatusResult(resultFile string, id int, prNumber, genre, name, state string) error {
	out := map[string]string{
		"statusId":    strconv.Itoa(id),
		"statusGenre": genre,
		"statusName":  name,
		"state":       state,
		// Re-emit prNumber so a single-hop inputsFrom on the immediately
		// following stage (ci-poll) can thread it through this stage — outputs
		// resolve against the preceding task only.
		"prNumber": prNumber,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal status result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}
