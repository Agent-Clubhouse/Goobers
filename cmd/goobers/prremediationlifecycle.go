package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// prClaimProvider is the narrow surface pr-claim needs: a single-PR poll to
// verify the claimed PR's live state and source head. Unlike
// remediationProvider (the full pr-remediation lane surface, GitHub/Gitea-
// only), GetPullRequest alone is a call every registered provider —
// including ADO's GetPullRequest, a thin adapter over its PollPullRequest
// (providers/ado_prthreads.go) — implements through the shared stage-provider
// seam, so pr-claim's terminal-state and revision guards run on GitHub, Gitea
// and ADO alike (#5655, #6128).
type prClaimProvider interface {
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
}

const prRemediationLifecycleResultFile = "pr-remediation-lifecycle.json"

const prRemediationLifecycleHelp = "Usage: goobers pr-claim [--release] [path]\n\n" +
	"At a pr-remediation stage boundary, verify that this run's claimed pull\n" +
	"request is still open and still at the exact source revision this run\n" +
	"selected (or itself published). If it has merged or closed, release the\n" +
	"claim and return a terminal no-work result; if it moved to a different\n" +
	"head, release the claim and return a distinct stale-selection no-work\n" +
	"result, so the runner stops the workflow either way. A missing or\n" +
	"malformed head fails closed.\n" +
	"With --release, explicitly release the run's PR claim without querying the\n" +
	"provider. Releasing an already-released claim is an idempotent success.\n\n" +
	"Exit codes: 0 = PR current, terminal/stale no-work, or released;\n" +
	"1 = business error; 2 = usage/IO error.\n"

// Outcomes pr-claim reports in its result's "outcome" field. terminal and
// stale_selection both end the workflow as no-work, and stay distinguishable
// here and in stdout: a stale selection is live work at a revision this run
// does not hold, never a closed PR (#6128).
const (
	prClaimOutcomeOpen           = "open"
	prClaimOutcomeTerminal       = "terminal"
	prClaimOutcomeStaleSelection = string(prRevisionStale)
	prClaimOutcomeReleased       = "released"
	prClaimOutcomeNoClaim        = "no_claim"
)

type prRemediationLifecycleResult struct {
	SelectedNumber string `json:"selectedNumber,omitempty"`
	Open           bool   `json:"open"`
	Released       bool   `json:"released"`
	NoWork         bool   `json:"noWork,omitempty"`
	// Outcome names which of the lifecycle's results this is (#6128).
	Outcome string `json:"outcome,omitempty"`
	// StaleSelection is true exactly when Outcome is stale_selection.
	StaleSelection bool `json:"staleSelection,omitempty"`
	// Revision is the revision check's state: current, stale_selection, or
	// unrecorded when this run holds no recorded selection to compare with.
	Revision string `json:"revision,omitempty"`
	// RevisionSource says where ExpectedHeadSHA came from: this run's
	// selection or its own publication.
	RevisionSource  string `json:"revisionSource,omitempty"`
	ExpectedHeadSHA string `json:"expectedHeadSha,omitempty"`
	LiveHeadSHA     string `json:"liveHeadSha,omitempty"`
	NoWorkReason    string `json:"noWorkReason,omitempty"`
}

func runPRRemediationLifecycle(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("pr-claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "pr-claim")
	release := fs.Bool("release", false, "release this run's PR claim without checking provider state")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	number, held, err := claimedPullRequestNumber(root)
	if err != nil {
		return failProviderStage(stderr, "read remediation PR claim", err, prRemediationLifecycleResultFile)
	}
	if *release {
		if err := releasePRRemediationClaim(root); err != nil {
			return failProviderStage(stderr, "release remediation PR claim", err, prRemediationLifecycleResultFile)
		}
		result := prRemediationLifecycleResult{Released: held, Outcome: prClaimOutcomeReleased}
		if held {
			result.SelectedNumber = strconv.Itoa(number)
		}
		return writePRRemediationLifecycleResult(result, stdout, stderr)
	}
	if !held {
		return writePRRemediationLifecycleResult(prRemediationLifecycleResult{
			Released: true,
			NoWork:   true,
			Outcome:  prClaimOutcomeNoClaim,
		}, stdout, stderr)
	}
	return verifyClaimedPullRequest(root, number, stdout, stderr)
}

// verifyClaimedPullRequest is pr-claim's guard: one live read of the claimed
// PR, compared with the revision this run recorded for it.
func verifyClaimedPullRequest(root string, number int, stdout, stderr io.Writer) int {
	repo, err := providerRepo(root)
	if err != nil {
		return failProviderStage(stderr, "load remediation repository", err, prRemediationLifecycleResultFile)
	}
	runID, _, err := providerRunContext()
	if err != nil {
		return failProviderStage(stderr, "read run context", err, prRemediationLifecycleResultFile)
	}
	expected, recorded, err := loadPRExpectedRevision(root, runID, repo, number)
	if err != nil {
		return failPRRevision(stderr, "load claimed pull request revision", err, prRemediationLifecycleResultFile)
	}
	pr, err := readClaimedPullRequest(root, repo, number)
	if err != nil {
		return failProviderStage(stderr, "refresh remediation pull request", err, prRemediationLifecycleResultFile)
	}
	check, err := evaluatePRRevision(expected, recorded, pr)
	if err != nil {
		return failPRRevision(stderr, "refresh remediation pull request", err, prRemediationLifecycleResultFile)
	}
	result := prRemediationLifecycleResult{
		SelectedNumber:  strconv.Itoa(number),
		Revision:        string(check.State),
		RevisionSource:  expected.Source,
		ExpectedHeadSHA: check.Expected,
		LiveHeadSHA:     check.Live,
	}
	if check.State == prRevisionTerminal || check.State == prRevisionStale {
		return endClaimedPullRequest(root, result, check, stdout, stderr)
	}
	result.Open = true
	result.Outcome = prClaimOutcomeOpen
	return writePRRemediationLifecycleResult(result, stdout, stderr)
}

// readClaimedPullRequest polls the claimed PR through the narrow surface every
// provider implements. Every provider, Azure DevOps included, polls with the
// github:pr:write credential pr-claim's manifest row declares (ADO-N18).
func readClaimedPullRequest(root string, repo providers.RepositoryRef, number int) (providers.PullRequestSummary, error) {
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return providers.PullRequestSummary{}, err
	}
	provider, err := remediationStageSurface[prClaimProvider](root, repo, token, withStageProviderCapability(capability.GitHubPRWrite))
	if err != nil {
		return providers.PullRequestSummary{}, err
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	return provider.GetPullRequest(ctx, repo, strconv.Itoa(number))
}

// endClaimedPullRequest releases the claim and reports no-work for a terminal
// or stale-selected PR, so the runner stops the workflow before any
// downstream stage can act on it. Labels are untouched: a stale selection
// keeps whatever made the PR eligible, so the next cycle re-selects it at its
// new head rather than this run adopting a revision it never selected.
func endClaimedPullRequest(root string, result prRemediationLifecycleResult, check prRevisionCheck, stdout, stderr io.Writer) int {
	if err := releasePRRemediationClaim(root); err != nil {
		return failProviderStage(stderr, "release remediation PR claim", err, prRemediationLifecycleResultFile)
	}
	result.Released = true
	result.NoWork = true
	if check.State == prRevisionStale {
		result.Open = true
		result.Outcome = prClaimOutcomeStaleSelection
		result.StaleSelection = true
		result.NoWorkReason = fmt.Sprintf("stale selection: PR #%s moved from this run's %s head %s to %s",
			result.SelectedNumber, result.RevisionSource, check.Expected, check.Live)
	} else {
		result.Outcome = prClaimOutcomeTerminal
		result.Revision = ""
		result.NoWorkReason = fmt.Sprintf("PR #%s is no longer open", result.SelectedNumber)
	}
	return writePRRemediationLifecycleResult(result, stdout, stderr)
}

func releasePRRemediationClaim(root string) error {
	runID, _, err := providerRunContext()
	if err != nil {
		return err
	}
	l := layoutFor(root)
	log, closeLog, err := claimLedgerJournal(l)
	if err != nil {
		return err
	}
	defer closeLog()
	return releasePullRequestClaimsForRun(l, log, runID)
}

func writePRRemediationLifecycleResult(result prRemediationLifecycleResult, stdout, stderr io.Writer) int {
	data, err := json.Marshal(result)
	if err != nil {
		pf(stderr, "error: marshal PR remediation lifecycle result: %v\n", err)
		return 2
	}
	resultFile := providerInput("resultFile", prRemediationLifecycleResultFile)
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	if result.NoWork {
		switch {
		case result.SelectedNumber == "":
			pln(stdout, "no work: this run holds no PR claim")
		case result.StaleSelection:
			pf(stdout, "no work: %s; claim released so the next cycle re-selects it\n", result.NoWorkReason)
		default:
			pf(stdout, "no work: this run's claimed PR #%s is no longer open; claim released\n", result.SelectedNumber)
		}
		return 0
	}
	if result.Released {
		if result.SelectedNumber == "" {
			pln(stdout, "PR claim already released; nothing to do")
		} else {
			pf(stdout, "released claim for PR #%s\n", result.SelectedNumber)
		}
		return 0
	}
	if result.Revision == string(prRevisionCurrent) {
		pf(stdout, "claimed PR #%s is still open at this run's %s head %s\n", result.SelectedNumber, result.RevisionSource, result.LiveHeadSHA)
		return 0
	}
	pf(stdout, "claimed PR #%s is still open\n", result.SelectedNumber)
	return 0
}
