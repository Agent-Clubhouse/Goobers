package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

func runOpenPR(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("open-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers open-pr [path]\n\n"+
			"Open the run's PR — or, on a repass through this stage, find and update\n"+
			"the PR it already opened (idempotent: the run's branch name is stable\n"+
			"across repasses, providers.BranchName). Writes prNumber/pull-request-url\n"+
			"to the declared result file for a downstream stage's Task.InputsFrom.\n"+
			"Exit codes: 0 = opened/updated, 1 = business error, 2 = usage/IO error.\n")
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
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	head := providerInput("head", providers.BranchName(workflow, runID))
	base := providerInput("base", "main")

	// Issue linkage (#241): derive the PR title from the claimed issue and add a
	// `Fixes #N` back-reference, so a human triaging several loop PRs can tell
	// them apart and the issue<->PR breadcrumb the #30 runbook checks exists on
	// both sides. Recovered from the run journal (resume-safe), so this holds on
	// a repass too. Falls back to the generic title/body when the run claimed no
	// issue (other workflows) or an explicit title/body input is set.
	issueID, issueTitle, haveIssue := claimedIssueFromJournal(root, runID)
	title := providerInput("title", "")
	if title == "" {
		if haveIssue && issueTitle != "" {
			title = issueTitle
		} else {
			title = "Automated implementation"
		}
	}
	body := providerInput("body", "")
	if body == "" {
		body = "Automated PR opened by the goobers implementation workflow."
	}
	if haveIssue && issueID != "" {
		body += "\n\nFixes #" + issueID
	}

	// Config write-boundary (#104/T4, wired here per #223). Opt-in and no-op by
	// default, so implementation/work-nomination are unaffected. When the Tutor
	// workflow sets confineToConfigRoot=true, every file this run's branch changes
	// (relative to base) must be within the configured config root — else the
	// cycle is aborted CLOSED before the PR is opened, so a self-improvement run
	// can never open a PR touching platform code.
	if providerInput("confineToConfigRoot", "") == "true" {
		if err := confineDiffToConfigRoot(base, providerInput("configRoot", "")); err != nil {
			pf(stderr, "error: config write-boundary: %v\n", err)
			return 1
		}
	}

	prReq := providers.PullRequestRequest{Repository: repo, Title: title, Body: body, Head: head, Base: base}
	if providerInput("runIdFooter", "true") == "true" {
		prReq.RunID = runID
	}

	result, err := provider.OpenPullRequest(context.Background(), prReq)
	if err != nil {
		pf(stderr, "error: open pull request: %v\n", err)
		return 1
	}

	resultFile := providerInput("resultFile", "pr-result.json")
	data, err := json.Marshal(map[string]string{
		"prNumber":         strconv.Itoa(result.Number),
		"pull-request-url": result.URL,
	})
	if err != nil {
		pf(stderr, "error: marshal pr result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "pr #%d: %s\n", result.Number, result.URL)
	return 0
}
