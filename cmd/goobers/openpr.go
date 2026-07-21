package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

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
	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))

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
	structuredBody := false
	if body == "" {
		body, structuredBody, err = renderStructuredPRBody(root, runID, issueID, issueTitle)
		if err != nil {
			pf(stderr, "error: render pull request body from journal: %v\n", err)
			return 1
		}
		if !structuredBody {
			body = "Automated PR opened by the goobers implementation workflow."
		}
	}
	if haveIssue && issueID != "" && !structuredBody {
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

	// Docs write-boundary (#1016). The docs-updater analog of the config
	// boundary above: opt-in (confineToDocsRoots=true) and no-op by default, so
	// every other workflow is unaffected. When set, every file this run's branch
	// changes must be within at least one declared in-repo docs root (the
	// ordered WorkflowSpec.docsRoots list, passed through as a comma/newline
	// `docsRoots` input) — else the cycle aborts CLOSED before the PR opens, so a
	// docs run can never open a PR touching code. An empty docsRoots list with
	// the boundary enabled fails closed (configboundary.ErrNoDocsRoots), never
	// silently allowing the whole tree.
	if providerInput("confineToDocsRoots", "") == "true" {
		if err := confineDiffToDocsRoots(base, parseDocsRoots(providerInput("docsRoots", ""))); err != nil {
			pf(stderr, "error: docs write-boundary: %v\n", err)
			return 1
		}
	}

	resultFile := providerInput("resultFile", "pr-result.json")

	// Mid-flight staleness re-check (#947). The claimed issue was validated
	// once, at query-backlog; but implement + review + local-ci can take 30+
	// minutes, and an issue closed or superseded in that window must NOT still
	// produce a PR — that burns a full merge-review cycle and one of the
	// scarcest resources there is (an open-PR slot) on work that is already
	// moot. Re-check that the claimed issue is still open, immediately before
	// opening the PR. The gate downstream (open-pr-gate) routes opened=false to
	// @abort so the run terminates with a clear, distinguishable reason instead
	// of a stale PR. Fail OPEN on any lookup error — a transient provider
	// failure must never block a legitimate PR — and gate on haveIssue so
	// issue-less runs (other workflows, generic PRs) keep exactly today's
	// behavior.
	if haveIssue && issueID != "" {
		ctxCheck, cancelCheck := providerCommandContext()
		item, checkErr := provider.GetWorkItem(ctxCheck, repo, issueID)
		cancelCheck()
		switch {
		case checkErr != nil:
			pf(stderr, "warning: could not re-check issue #%s state before opening PR (%v) — proceeding\n", issueID, checkErr)
		case item.State != "" && !strings.EqualFold(item.State, "open"):
			pf(stdout, "issue #%s is no longer open (state %q) since it was claimed — aborting without opening a PR (#947)\n", issueID, item.State)
			if err := writeOpenPRResult(resultFile, false, 0, ""); err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			return 0
		}
	}

	prReq := providers.PullRequestRequest{Repository: repo, Title: title, Body: body, Head: head, Base: base}
	if providerInput("runIdFooter", "true") == "true" {
		prReq.RunID = runID
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	result, err := provider.OpenPullRequest(ctx, prReq)
	if err != nil {
		return failProviderStage(stderr, "open pull request", err, "pr-result.json")
	}

	if err := writeOpenPRResult(resultFile, true, result.Number, result.URL); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	pf(stdout, "pr #%d: %s\n", result.Number, result.URL)
	return 0
}

// writeOpenPRResult writes open-pr's declared result file. It always emits the
// `opened` flag the open-pr-gate routes on (#947); prNumber/pull-request-url
// are present only on the opened path (ci-poll reads them via inputsFrom, and
// ci-poll only runs when opened=true).
func writeOpenPRResult(resultFile string, opened bool, prNumber int, url string) error {
	out := map[string]string{"opened": strconv.FormatBool(opened)}
	if opened {
		out["prNumber"] = strconv.Itoa(prNumber)
		out["pull-request-url"] = url
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal pr result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}
