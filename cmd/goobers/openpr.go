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
	title := providerInput("title", "Automated implementation")
	body := providerInput("body", "Automated PR opened by the goobers implementation workflow.")

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
