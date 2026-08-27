package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// openPRProvider is the narrow surface open-pr needs: the mid-flight issue
// staleness re-check (GetWorkItem) and the PR open/update itself. Both the
// GitHub and ADO providers satisfy it, so open-pr is provider-neutral once the
// backend is resolved from instance config.
type openPRProvider interface {
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
	OpenPullRequest(context.Context, providers.PullRequestRequest) (providers.PullRequestResult, error)
}

const openPRHelp = "Usage: goobers open-pr [path]\n\n" +
	"Open the run's PR — or, on a repass through this stage, find and update\n" +
	"the PR it already opened (idempotent: the run's branch name is stable\n" +
	"across repasses, providers.BranchName). Writes prNumber/pull-request-url\n" +
	"to the declared result file for a downstream stage's Task.InputsFrom.\n" +
	"Exit codes: 0 = opened/updated, 1 = business error, 2 = usage/IO error.\n"

func runOpenPR(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("open-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "open-pr")
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
	stageProvider, err := newProviderForStage(root, repo, false,
		withStageProviderCapability(capability.ProviderPRWrite),
		withStageProviderMutations("pr"),
		withStageProviderOpenPR(),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := openPRProvider(stageProvider)

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	head := providerInput("head", providers.BranchNameIn(providerBranchNamespace(), workflow, runID))
	base := providerInput("base", providerBaseBranch())

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
	runsDir, err := runsDirForRun(layoutFor(root), runID)
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		pf(stderr, "error: locate run journal for escalation state: %v\n", err)
		return 1
	}
	if err == nil {
		escalation, duplicate, err := issueCloseOutDuplicateEscalation(runsDir, runID)
		if err != nil {
			pf(stderr, "error: resolve duplicate-diff escalation: %v\n", err)
			return 1
		}
		if duplicate {
			body, err = withImplementationEscalationMarker(body, escalation)
			if err != nil {
				pf(stderr, "error: render duplicate-diff escalation: %v\n", err)
				return 1
			}
		}
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

	// Per-target-action-root write-boundary (TUT-A5/#1217). The Tutor's
	// per-action-class boundary: opt-in (confineToActionRoots=true) and no-op
	// by default. When set, every file this run's branch changes must resolve
	// into the SAME single declared action root (the comma/newline
	// `actionRoots` input, e.g. "reference-workflows,skills") — a skill-authoring action
	// cannot also rewrite a workflow, or vice versa — else the cycle aborts
	// CLOSED before the PR opens (configboundary.ConfineExclusive).
	if providerInput("confineToActionRoots", "") == "true" {
		if err := confineDiffToActionRoots(base, parseDocsRoots(providerInput("actionRoots", ""))); err != nil {
			pf(stderr, "error: action write-boundary: %v\n", err)
			return 1
		}
	}

	var tutorHoldout *tutorHoldoutRecord
	recordTutorLiveVerification := false
	if isTutorWorkflow(workflow) {
		changes, err := localTutorChanges(base)
		if err != nil {
			pf(stderr, "error: classify Tutor change: %v\n", err)
			return 1
		}
		classification, err := classifyTutorChanges(changes)
		if err != nil {
			pf(stderr, "error: classify Tutor change: %v\n", err)
			return 1
		}
		body = strings.TrimRight(body, "\n") + "\n\n" + tutorClassificationPRSection(classification)
		recordTutorLiveVerification = providerInput("recordLiveVerification", "") == "true"
		if recordTutorLiveVerification {
			tutorHoldout, err = prepareTutorHoldout(
				root,
				os.Getenv("GOOBERS_GAGGLE"),
				runID,
				providerInput("tutorConfigSource", ""),
				classification,
				changes,
				time.Now().UTC(),
			)
			if err != nil {
				pf(stderr, "error: prepare Tutor live verification: %v\n", err)
				return 1
			}
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
			if err := writeOpenPRResult(resultFile, false, 0, "", ""); err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			return 0
		}
	}

	// Persist the mandatory finding before the external mutation. If the
	// process crashes after GitHub accepts the PR, the prepared record still
	// survives for a later exact-cohort verification pass. Repasses atomically
	// replace this run-keyed file; optional final classifications remove it.
	if recordTutorLiveVerification {
		if tutorHoldout == nil {
			if err := clearTutorHoldoutsForRun(root, os.Getenv("GOOBERS_GAGGLE"), runID); err != nil {
				pf(stderr, "error: replace Tutor live verification: %v\n", err)
				return 1
			}
		} else if err := writeTutorHoldout(root, *tutorHoldout); err != nil {
			pf(stderr, "error: prepare Tutor live verification: %v\n", err)
			return 1
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
		if tutorHoldout != nil {
			if cleanupErr := clearTutorHoldoutsForRun(root, tutorHoldout.Gaggle, tutorHoldout.AuthoringRunID); cleanupErr != nil {
				pf(stderr, "error: discard Tutor live verification after open pull request failed: %v (open pull request: %v)\n", cleanupErr, err)
				return 1
			}
		}
		return failProviderStage(stderr, "open pull request", err, "pr-result.json")
	}

	if recordTutorLiveVerification {
		if tutorHoldout != nil {
			tutorHoldout.PRNumber = result.Number
			tutorHoldout.PRURL = result.URL
			if err := writeTutorHoldout(root, *tutorHoldout); err != nil {
				pf(stderr, "error: record Tutor live verification: %v\n", err)
				return 1
			}
		}
	}

	if err := writeOpenPRResult(resultFile, true, result.Number, result.URL, title); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	// Gate-edit review routing (TUT-A3, #1215): a tutor run whose
	// gate-removal-guard stage classified this diff as removing or loosening
	// its own flagged gate gets the stricter-review label; ordinary gate
	// tuning gets the lighter one. Best-effort — labeling failures never fail
	// an already-opened PR, same posture as flagScopeDrift.
	if kind, subject := gateEditClassificationFromJournal(root, runID); kind != "" && kind != "none" {
		if gh, ok := provider.(*providers.GitHubProvider); ok {
			if lerr := labelGateEdit(ctx, gh, repo, result.Number, kind, subject); lerr != nil {
				pf(stderr, "warning: could not label pr #%d for gate-edit review routing (%v)\n", result.Number, lerr)
			}
		}
	}

	pf(stdout, "pr #%d: %s\n", result.Number, result.URL)
	return 0
}

// writeOpenPRResult writes open-pr's declared result file. It always emits the
// `opened` flag the open-pr-gate routes on (#947); PR facts are present only
// on the opened path (ci-poll reads prNumber/pull-request-url via inputsFrom,
// and the run projection correlates id/title for portal display).
func writeOpenPRResult(resultFile string, opened bool, prNumber int, url, title string) error {
	out := map[string]string{"opened": strconv.FormatBool(opened)}
	if opened {
		out["prNumber"] = strconv.Itoa(prNumber)
		out["pull-request-url"] = url
		out["id"] = strconv.Itoa(prNumber)
		out["title"] = title
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
