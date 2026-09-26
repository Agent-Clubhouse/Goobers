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
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
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

type adoPullRequestWorkItemLinker interface {
	LinkPullRequestToWorkItem(context.Context, providers.RepositoryRef, providers.RepositoryRef, string, string) error
}

func openPRWorkItemLinker(root string, repo providers.RepositoryRef, haveIssue bool, issueID string) (adoPullRequestWorkItemLinker, error) {
	if repo.Provider != providers.ProviderADO || !haveIssue || issueID == "" {
		return nil, nil
	}
	return newProviderForStageSurface[adoPullRequestWorkItemLinker](root, repo, false,
		withStageProviderCapability(capability.ADOWorkItemsWrite),
		withStageProviderMutations("issue"),
	)
}

func linkADOPullRequestToWorkItem(
	ctx context.Context,
	linker adoPullRequestWorkItemLinker,
	repo providers.RepositoryRef,
	root, issueID, pullID string,
	haveIssue bool,
	stderr io.Writer,
) int {
	if repo.Provider != providers.ProviderADO || !haveIssue || issueID == "" {
		return 0
	}
	if linker == nil {
		pf(stderr, "error: ADO provider cannot create native work-item links\n")
		return 1
	}
	err := linker.LinkPullRequestToWorkItem(ctx, repo, backlogRepoRefForStage(root, repo), issueID, pullID)
	if err == nil {
		return 0
	}
	if providers.IsNotFoundError(err) {
		pf(stderr, "warning: work item #%s no longer resolves; pull request %s could not be linked natively\n", issueID, pullID)
		return 0
	}
	return failProviderStage(stderr, "link pull request to work item", err, "pr-result.json")
}

func openPullRequestWithADOLink(
	ctx context.Context,
	provider openPRProvider,
	linker adoPullRequestWorkItemLinker,
	repo providers.RepositoryRef,
	root, issueID string,
	haveIssue bool,
	prReq providers.PullRequestRequest,
	tutorHoldout *tutorHoldoutRecord,
	stderr io.Writer,
) (providers.PullRequestResult, int) {
	result, err := provider.OpenPullRequest(ctx, prReq)
	if err != nil {
		if tutorHoldout != nil {
			if cleanupErr := clearTutorHoldoutsForRun(root, tutorHoldout.Gaggle, tutorHoldout.AuthoringRunID); cleanupErr != nil {
				pf(stderr, "error: discard Tutor live verification after open pull request failed: %v (open pull request: %v)\n", cleanupErr, err)
				return providers.PullRequestResult{}, 1
			}
		}
		return providers.PullRequestResult{}, failProviderStage(stderr, "open pull request", err, "pr-result.json")
	}
	return result, linkADOPullRequestToWorkItem(ctx, linker, repo, root, issueID, result.ID, haveIssue, stderr)
}

const openPRHelp = "Usage: goobers open-pr [path]\n\n" +
	"Open the run's PR — or, on a repass through this stage, find and update\n" +
	"the PR it already opened (idempotent: the run's branch name is stable\n" +
	"across repasses, providers.BranchName). Writes prNumber/pull-request-url\n" +
	"to the declared result file for a downstream stage's Task.InputsFrom.\n\n" +
	"Inputs (Task.Inputs / inputsFrom): title, body, head (default the run's\n" +
	"stable branch), base (default GOOBERS_BASE_BRANCH, else \"main\"), itemID,\n" +
	"itemTitle, resultFile, timeout. PR metadata is configured through these\n" +
	"workflow\n" +
	"inputs — there are no --title/--body flags — and a stage may bind them\n" +
	"from an upstream stage's declared output with inputsFrom rather than a\n" +
	"static value:\n\n" +
	"    - name: open-pr\n" +
	"      run:\n" +
	"        command: [\"goobers\", \"open-pr\"]\n" +
	"      inputs:\n" +
	"        resultFile: \"pr-result.json\"\n" +
	"      inputsFrom:\n" +
	"        # bare key = the immediately preceding stage's output;\n" +
	"        # \"plan.prTitle\" would name an earlier stage explicitly.\n" +
	"        title: prTitle\n\n" +
	"Title precedence: an explicitly set non-empty title wins; otherwise the\n" +
	"claimed item's title, recovered from the run journal (so it survives a\n" +
	"resume or repass); otherwise the generic \"Automated implementation\". An\n" +
	"empty value is not an override — every empty input falls back.\n\n" +
	"itemID explicitly identifies a selected backlog item when the workflow\n" +
	"read it without claiming. If a claimed item also exists, the IDs must\n" +
	"match. On ADO, native work-item linking separately requires the\n" +
	"ado:work-items:write capability; GitHub never resolves that capability.\n\n" +
	"Body precedence: an explicitly set non-empty body is used as given and\n" +
	"bypasses structured rendering; otherwise a structured body is rendered\n" +
	"from the run journal's recorded review and local-CI evidence; otherwise a\n" +
	"generic one-line body. A claimed item still augments an unstructured body\n" +
	"— explicit or generic — with a \"Fixes #<id>\" back-reference, so explicit\n" +
	"body text does not cost the issue linkage. The structured body carries\n" +
	"its own linkage and is never appended to.\n\n" +
	"A workflow that claims no item, or whose journal holds no recognized\n" +
	"review/local-CI evidence, therefore gets generic metadata unless it sets\n" +
	"these inputs. That is the fallback working, not a missing feature.\n" +
	"Exit codes: 0 = opened/updated, 1 = business error, 2 = usage/IO error.\n"

func openPRIssue(root, runID string) (id, title string, ok bool, err error) {
	id, title, ok = claimedIssueFromJournal(root, runID)
	explicitID := strings.TrimSpace(providerInput("itemID", ""))
	explicitTitle := strings.TrimSpace(providerInput("itemTitle", ""))
	if explicitID == "" {
		if explicitTitle != "" && !ok {
			return "", "", false, fmt.Errorf("open-pr input itemTitle requires itemID when the run has no claimed item")
		}
		if explicitTitle != "" {
			title = explicitTitle
		}
		return id, title, ok, nil
	}
	if ok && id != explicitID {
		return "", "", false, fmt.Errorf("open-pr input itemID %q conflicts with claimed item %q", explicitID, id)
	}
	id, ok = explicitID, true
	if explicitTitle != "" {
		title = explicitTitle
	}
	return id, title, ok, nil
}

func openPRTitle(root, runID string) (title, issueID, issueTitle string, haveIssue bool, err error) {
	issueID, issueTitle, haveIssue, err = openPRIssue(root, runID)
	if err != nil {
		return "", "", "", false, err
	}
	title = providerInput("title", "")
	if title == "" && haveIssue {
		title = issueTitle
	}
	if title == "" {
		title = "Automated implementation"
	}
	return title, issueID, issueTitle, haveIssue, nil
}

func runOpenPR(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("open-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "open-pr")
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

	head := providerInput("head", preferredOpenPRHead(root, runID, workflow))
	base := providerInput("base", providerBaseBranch())

	// Issue linkage (#241): derive the PR title from the claimed issue and add a
	// `Fixes #N` back-reference, so a human triaging several loop PRs can tell
	// them apart and the issue<->PR breadcrumb the #30 runbook checks exists on
	// both sides. Recovered from the run journal (resume-safe), so this holds on
	// a repass too. Falls back to the generic title/body when the run claimed no
	// issue (other workflows) or an explicit title/body input is set.
	title, issueID, issueTitle, haveIssue, err := openPRTitle(root, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
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
	_, journalErr := stageRunJournal(root, runID)
	if journalErr != nil && !errors.Is(journalErr, journalclient.ErrRunNotFound) {
		pf(stderr, "error: locate run journal for escalation state: %v\n", journalErr)
		return 1
	}
	if journalErr == nil {
		escalation, duplicate, err := issueCloseOutDuplicateEscalation(root, runID)
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
				os.Getenv(executor.GaggleEnvVar),
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
	//
	// The read must target the project the work item actually lives in (#3648).
	// On ADO the backlog is a different project from the code repo the branch
	// and PR land in, so a GetWorkItem addressed at the routed code repo returns
	// not-found for every claimed item — and because the check fails open, that
	// silently reinstates exactly the stale PR #947 exists to prevent. Only the
	// work-item read is re-addressed (backlogRepoRefForStage); the PR itself
	// still opens against the routed code repository.
	if haveIssue && issueID != "" {
		issuesRepo := backlogRepoRefForStage(root, repo)
		ctxCheck, cancelCheck := providerCommandContext()
		item, checkErr := provider.GetWorkItem(ctxCheck, issuesRepo, issueID)
		cancelCheck()
		switch {
		case providers.IsNotFoundError(checkErr):
			pf(stderr, "warning: claimed issue #%s does not resolve in %s — the staleness re-check could not confirm it is still open; proceeding\n",
				issueID, repositoryDisplayName(issuesRepo))
		case checkErr != nil:
			pf(stderr, "warning: could not re-check issue #%s state in %s before opening PR (%v) — proceeding\n",
				issueID, repositoryDisplayName(issuesRepo), checkErr)
		case item.State != "" && !strings.EqualFold(item.State, "open"):
			pf(stdout, "issue #%s is no longer open (state %q) since it was claimed — aborting without opening a PR (#947)\n", issueID, item.State)
			if err := writeOpenPRResult(resultFile, false, 0, ""); err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			return 0
		}
	}

	workItemLinker, err := openPRWorkItemLinker(root, repo, haveIssue, issueID)
	if err != nil {
		pf(stderr, "error: resolve ADO work-item link authority: %v\n", err)
		return 1
	}

	// Persist the mandatory finding before the external mutation. If the
	// process crashes after GitHub accepts the PR, the prepared record still
	// survives for a later exact-cohort verification pass. Repasses atomically
	// replace this run-keyed file; optional final classifications remove it.
	if recordTutorLiveVerification {
		if tutorHoldout == nil {
			if err := clearTutorHoldoutsForRun(root, os.Getenv(executor.GaggleEnvVar), runID); err != nil {
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
	result, code := openPullRequestWithADOLink(ctx, provider, workItemLinker, repo, root, issueID, haveIssue, prReq, tutorHoldout, stderr)
	if code != 0 {
		return code
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

	if err := writeOpenPRResult(resultFile, true, result.Number, result.URL); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	// Gate-edit review routing (TUT-A3, #1215): a tutor run whose
	// gate-removal-guard stage classified this diff as removing or loosening
	// its own flagged gate gets the stricter-review label; ordinary gate
	// tuning gets the lighter one. Best-effort — labeling failures never fail
	// an already-opened PR, same posture as flagScopeDrift.
	kind, subject, classifyErr := gateEditClassificationFromJournal(root, runID)
	switch {
	case classifyErr != nil:
		// Loud, not silent: an unreadable journal means we do not KNOW whether
		// this diff edits its own gate, and the PR is going out unlabelled.
		pf(stderr, "warning: could not read gate-edit classification for pr #%d from the run journal (%v) — the pr is unlabelled for gate-edit review routing\n",
			result.Number, classifyErr)
	case kind != "" && kind != "none":
		if gh, ok := provider.(*providers.GitHubProvider); ok {
			if lerr := labelGateEdit(ctx, gh, repo, result.Number, kind, subject); lerr != nil {
				pf(stderr, "warning: could not label pr #%d for gate-edit review routing (%v)\n", result.Number, lerr)
			}
		}
	}

	pf(stdout, "pr #%d: %s\n", result.Number, result.URL)
	return 0
}

func preferredOpenPRHead(root, runID, workflow string) string {
	if branch, ok := runIdentityBranch(root, runID); ok {
		return branch
	}
	if branch, ok := runBranchFromJournal(root, runID); ok {
		return branch
	}
	return providers.BranchNameIn(providerBranchNamespace(), workflow, runID)
}

func runIdentityBranch(root, runID string) (string, bool) {
	selection, err := stageJournalSelection()
	if err == nil && selection.OnPlane() {
		if branch := strings.TrimSpace(os.Getenv("GOOBERS_WORKSPACE_BRANCH")); branch != "" {
			return branch, true
		}
		return "", false
	}
	dir, err := layoutFor(root).FindRunDir(runID)
	if err != nil {
		return "", false
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return "", false
	}
	identity, err := reader.Identity()
	if err != nil {
		return "", false
	}
	branch := strings.TrimSpace(identity.WorkspaceBranch)
	return branch, branch != ""
}

func runBranchFromJournal(root, runID string) (string, bool) {
	reader, err := stageRunJournal(root, runID)
	if err != nil {
		return "", false
	}
	events, err := reader.Events()
	if err != nil {
		return "", false
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventRefTouched || event.ExternalRef == nil || event.ExternalRef.Kind != "branch" {
			continue
		}
		branch := strings.TrimSpace(event.ExternalRef.ID)
		if branch != "" {
			return branch, true
		}
	}
	return "", false
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
