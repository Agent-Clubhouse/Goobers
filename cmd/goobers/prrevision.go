package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/providers"
)

// prrevision.go is the shared pull-request revision precondition (#6128).
//
// A pr-remediation run selects one pull request at one exact source revision:
// gather-pr-context claims the PR and records the head SHA it selected in its
// own result artifact (the remediation brief's gatherPrContext.headSha, a
// required field in every brief wire version). Everything the run derives
// afterwards — the rebase, the checkpoint's lease expectation, the agent's
// context, the reviewer's verdict, local CI — is derived from THAT revision.
// So "the PR is still open" is not enough to continue: a PR that moved to a
// different valid revision is still open, and work derived from the old one
// must not continue against it.
//
// The expectation lives in the run journal, not in process memory, so it
// survives a daemon restart, a workflow resume and a stage retry unchanged.
// It changes only when this run makes a new selection (a later
// gather-pr-context artifact) or itself publishes a new head through
// push-remediated's force-with-lease — the one legitimate way the PR's head
// moves under a run without invalidating the run's work. Git's own lease and
// resolve-review-threads' published-head checks stay in place; this contract
// adds the check every boundary before them was missing.

const (
	// prRevisionSourceSelection: the expectation is the head gather-pr-context
	// selected and claimed.
	prRevisionSourceSelection = "selection"
	// prRevisionSourcePublication: the expectation is the head this run's own
	// push-remediated published (and verified through its lease).
	prRevisionSourcePublication = "publication"

	prSelectionStage   = "gather-pr-context"
	prPublicationStage = "push-remediated"
	prRebaseStage      = "rebase-pr"

	// rebasePushedHeadOutput is rebase-pr's record of the head it force-pushed
	// when it continues into the agentic chain (empty when it pushed nothing).
	rebasePushedHeadOutput = "pushedHeadSha"
)

// prRevisionState classifies a claimed PR against the run's expectation.
type prRevisionState string

const (
	// prRevisionCurrent: open, unmerged, and exactly at the expected head.
	prRevisionCurrent prRevisionState = "current"
	// prRevisionStale: open and unmerged, but at a DIFFERENT head than the
	// one this run selected. Distinct from terminal: the PR is still live
	// work, just not the work this run is holding.
	prRevisionStale prRevisionState = "stale_selection"
	// prRevisionTerminal: merged, closed, completed or abandoned.
	prRevisionTerminal prRevisionState = "terminal"
	// prRevisionUnrecorded: open, but this run recorded no selection to
	// compare against (a run that never ran gather-pr-context, or no local
	// journal at all). Only the open-state and head-presence checks apply.
	prRevisionUnrecorded prRevisionState = "unrecorded"
)

// prExpectedRevision is the immutable source revision a run expects its
// claimed pull request to sit at, with the PR identity it was recorded for.
type prExpectedRevision struct {
	Provider        providers.ProviderKind `json:"provider"`
	Repository      string                 `json:"repository"`
	PullRequest     string                 `json:"pullRequest"`
	ExpectedHeadSHA string                 `json:"expectedHeadSHA"`
	Source          string                 `json:"source"`
}

// prRevisionCheck is the outcome of comparing one live PR read with the
// expectation.
type prRevisionCheck struct {
	State    prRevisionState
	Expected string
	Live     string
}

// errPRRevisionUnverifiable marks a live read that cannot establish the PR's
// revision (no head, or a malformed one). Callers fail closed on it: it is
// neither "current" nor "stale", and guessing either would be unsafe.
var errPRRevisionUnverifiable = errors.New("pull request revision cannot be established")

// errorCodePRRevisionUnverifiable is the typed stage error for
// errPRRevisionUnverifiable and for a malformed recorded expectation.
const errorCodePRRevisionUnverifiable = "pr_revision_unverifiable"

// isFullCommitSHA reports whether s is a full SHA-1 or SHA-256 object name.
// An abbreviated SHA is ambiguous by construction, so it never satisfies the
// "exact revision" contract.
func isFullCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func pullRequestIsOpen(pr providers.PullRequestSummary) bool {
	return strings.EqualFold(pr.State, "open") && !pr.Merged
}

// evaluatePRRevision compares a live pull-request read with the expectation.
// Terminal state wins over revision: a merged PR is terminal whatever its
// head. A live open PR must report a head; when an expectation is recorded
// that head must be a full SHA and must equal it exactly (case-insensitively,
// since hex is case-insensitive and providers differ in case).
func evaluatePRRevision(expected prExpectedRevision, recorded bool, pr providers.PullRequestSummary) (prRevisionCheck, error) {
	live := strings.TrimSpace(pr.HeadSHA)
	check := prRevisionCheck{Expected: expected.ExpectedHeadSHA, Live: live}
	if !pullRequestIsOpen(pr) {
		check.State = prRevisionTerminal
		return check, nil
	}
	if live == "" {
		return check, fmt.Errorf("%w: pull request #%s has no source head SHA", errPRRevisionUnverifiable, expected.PullRequest)
	}
	if !recorded {
		check.State = prRevisionUnrecorded
		return check, nil
	}
	if !isFullCommitSHA(live) {
		return check, fmt.Errorf("%w: pull request #%s reports malformed source head %q", errPRRevisionUnverifiable, expected.PullRequest, live)
	}
	if !strings.EqualFold(live, expected.ExpectedHeadSHA) {
		check.State = prRevisionStale
		return check, nil
	}
	check.State = prRevisionCurrent
	return check, nil
}

// loadPRExpectedRevision recovers the revision this run expects PR number to
// sit at, from this run's own journal. recorded=false means the run has no
// selection to compare against (no gather-pr-context artifact, or no local
// journal for the run at all); every other inconsistency — a selection
// artifact naming a different PR, a missing or malformed head — is an error,
// because silently treating it as "unrecorded" would disable the guard.
func loadPRExpectedRevision(root, runID string, repo providers.RepositoryRef, number int) (prExpectedRevision, bool, error) {
	expected := prExpectedRevision{
		Provider: repo.Provider, Repository: repo.CanonicalKey(), PullRequest: strconv.Itoa(number),
	}
	rd, err := stageRunJournal(root, runID)
	if errors.Is(err, journalclient.ErrRunNotFound) {
		return expected, false, nil
	}
	if err != nil {
		return expected, false, upstreamArtifactUnreadable(prSelectionStage, "PR selection revision", err)
	}
	events, err := rd.Events()
	if err != nil {
		return expected, false, upstreamArtifactUnreadable(prSelectionStage, "PR selection revision", err)
	}
	head, source, err := latestPRRevisionRecord(rd, runID, events, number)
	if err != nil || head == "" {
		return expected, false, err
	}
	if !isFullCommitSHA(head) {
		return expected, false, fmt.Errorf("%w: this run's recorded %s head %q for PR #%d is not a full commit SHA",
			errPRRevisionUnverifiable, source, head, number)
	}
	expected.ExpectedHeadSHA = head
	expected.Source = source
	return expected, true, nil
}

// latestPRRevisionRecord scans events for the newest revision-defining record:
// a gather-pr-context result artifact (a new selection), a push-remediated
// stage that published (this run's own force-with-lease publication), or a
// rebase-pr stage that force-pushed its clean rebase and continued into the
// agentic chain. The later one in journal order wins, so a new selection
// resets the expectation and an own publication advances it. rebase-pr's push
// advances it only when it was built on the head this run already expected:
// rebase-pr leases against the head it checked out, which may itself be a
// human's push since selection, and adopting that would launder the human's
// commit into this run's expectation.
func latestPRRevisionRecord(rd journalclient.Reader, runID string, events []journal.Event, number int) (head, source string, err error) {
	for i := range events {
		event := events[i]
		if ref, ok := prSelectionArtifactRef(runID, event); ok {
			source = prRevisionSourceSelection
			if head, err = selectedHeadFromArtifact(rd, ref, number); err != nil || head == "" {
				head, source = "", ""
			}
			continue
		}
		if localHead, ok := prPublishedHead(event); ok {
			head, source, err = localHead, prRevisionSourcePublication, nil
			continue
		}
		if attempted, pushed, ok := prRebasePushedHead(event); ok && err == nil && head != "" && strings.EqualFold(attempted, head) {
			head, source = pushed, prRevisionSourcePublication
		}
	}
	if err != nil {
		return "", "", err
	}
	return head, source, nil
}

// prRebasePushedHead reads a rebase-pr stage that force-pushed its clean
// rebase: the head it leased against and the head it published.
func prRebasePushedHead(event journal.Event) (attempted, pushed string, ok bool) {
	if event.Type != journal.EventStageFinished || event.Stage != prRebaseStage {
		return "", "", false
	}
	pushed, _ = event.Outputs[rebasePushedHeadOutput].(string)
	attempted, _ = event.Outputs["attemptedHeadSha"].(string)
	pushed, attempted = strings.TrimSpace(pushed), strings.TrimSpace(attempted)
	return attempted, pushed, pushed != "" && attempted != ""
}

func prSelectionArtifactRef(runID string, event journal.Event) (journal.Ref, bool) {
	if event.Type != journal.EventArtifactRecorded || event.Ref == nil {
		return journal.Ref{}, false
	}
	if stageArtifactName(runID, event.Name) != prSelectionStage+"/result" {
		return journal.Ref{}, false
	}
	return *event.Ref, true
}

func prPublishedHead(event journal.Event) (string, bool) {
	if event.Type != journal.EventStageFinished || event.Stage != prPublicationStage {
		return "", false
	}
	if published, _ := event.Outputs[pushRemediatedPublishedOutput].(string); published != "true" {
		return "", false
	}
	localHead, _ := event.Outputs[pushRemediatedLocalHeadOutput].(string)
	localHead = strings.TrimSpace(localHead)
	return localHead, localHead != ""
}

// selectedHeadFromArtifact reads the head SHA gather-pr-context selected from
// its own result artifact. An artifact that is not a remediation brief (a
// no-work result) records no selection and yields "".
func selectedHeadFromArtifact(rd journalclient.Reader, ref journal.Ref, number int) (string, error) {
	data, err := rd.ArtifactBytes(ref)
	if err != nil {
		return "", upstreamArtifactUnreadable(prSelectionStage, "PR selection revision", err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		return "", upstreamArtifactUnreadable(prSelectionStage, "PR selection revision", err)
	}
	if !strings.HasPrefix(brief.Schema, "goobers.dev/remediation-brief/") {
		return "", nil
	}
	if brief.SelectedNumber != strconv.Itoa(number) {
		return "", fmt.Errorf("%w: this run's selection artifact names PR #%s but its claim holds PR #%d",
			errPRRevisionUnverifiable, brief.SelectedNumber, number)
	}
	head := strings.TrimSpace(brief.GatherPRContext.HeadSHA)
	if head == "" {
		return "", fmt.Errorf("%w: this run's selection of PR #%d recorded no head SHA", errPRRevisionUnverifiable, number)
	}
	return head, nil
}

// failPRRevision routes a revision-establishment error to the right typed
// failure. Unverifiable data fails closed as errorCodePRRevisionUnverifiable,
// non-retryable: it is not a provider error — the provider answered; its
// answer (or this run's record) cannot anchor a revision — so it must not be
// classified, retried or counted as one. Anything else (an unreadable
// journal, a provider error) keeps its own classification.
func failPRRevision(stderr io.Writer, what string, err error, resultFileDefault string) int {
	if errors.Is(err, errPRRevisionUnverifiable) {
		return failProviderStageWithCode(stderr, what, err, errorCodePRRevisionUnverifiable, resultFileDefault)
	}
	return failProviderStage(stderr, what, err, resultFileDefault)
}
