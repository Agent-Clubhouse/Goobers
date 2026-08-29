package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

const remediationEscalatedLabel = "goobers:merge-escalated"

const siblingOverlapLookback = 30 * 24 * time.Hour

type remediationCause string

const (
	remediationCauseConflict       remediationCause = remediateCauseConflict
	remediationCauseSubstantive    remediationCause = remediateCauseSubstantive
	remediationCauseFailingCI      remediationCause = remediateCauseFailingCI
	remediationCauseSiblingOverlap remediationCause = remediateCauseSiblingOverlap
	remediationCauseHumanComment   remediationCause = remediateCauseHumanComment
)

type remediationEscalationOutcome string

const (
	remediationOutcomeDidNotConverge  remediationEscalationOutcome = "did-not-converge"
	remediationOutcomeBudgetExhausted remediationEscalationOutcome = "budget-exhausted"
	remediationOutcomePolicyExcluded  remediationEscalationOutcome = "policy-excluded"
	remediationOutcomeInfrastructure  remediationEscalationOutcome = "infrastructure-failure"
)

var remediationCauseOrder = []remediationCause{
	remediationCauseConflict,
	remediationCauseSubstantive,
	remediationCauseFailingCI,
	remediationCauseSiblingOverlap,
	remediationCauseHumanComment,
}

type remediationAttempts struct {
	Conflict       int `json:"conflict,omitempty"`
	Substantive    int `json:"substantive,omitempty"`
	FailingCI      int `json:"failing-ci,omitempty"`
	SiblingOverlap int `json:"sibling-overlap,omitempty"`
	HumanComment   int `json:"human-comment,omitempty"`
}

func (a remediationAttempts) forCause(cause remediationCause) int {
	switch cause {
	case remediationCauseConflict:
		return a.Conflict
	case remediationCauseSubstantive:
		return a.Substantive
	case remediationCauseFailingCI:
		return a.FailingCI
	case remediationCauseSiblingOverlap:
		return a.SiblingOverlap
	case remediationCauseHumanComment:
		return a.HumanComment
	default:
		return 0
	}
}

func (a *remediationAttempts) increment(cause remediationCause) {
	switch cause {
	case remediationCauseConflict:
		a.Conflict++
	case remediationCauseSubstantive:
		a.Substantive++
	case remediationCauseFailingCI:
		a.FailingCI++
	case remediationCauseSiblingOverlap:
		a.SiblingOverlap++
	case remediationCauseHumanComment:
		a.HumanComment++
	}
}

type remediationBudgets struct {
	Conflict       int
	Substantive    int
	FailingCI      int
	SiblingOverlap int
	HumanComment   int
}

func (b remediationBudgets) forCause(cause remediationCause) int {
	switch cause {
	case remediationCauseConflict:
		return b.Conflict
	case remediationCauseSubstantive:
		return b.Substantive
	case remediationCauseFailingCI:
		return b.FailingCI
	case remediationCauseSiblingOverlap:
		return b.SiblingOverlap
	case remediationCauseHumanComment:
		return b.HumanComment
	default:
		return 0
	}
}

type siblingOverlapFinding struct {
	Number   int    `json:"number"`
	State    string `json:"state"`
	Message  string `json:"message"`
	Location string `json:"location,omitempty"`
}

// remediationState is pr-remediation's OWN durable per-PR loop-control
// state (D4's per-cause attempt counters + D5's last diff digest) — distinct
// from merge-review's Verdict payload (applyverdict.go's verdict-json), since
// it is written and read by a different workflow's runs. Embedded in a sticky
// PR comment the same way, and for the same reason: gather-pr-context
// already established that a PR comment is the only durable cross-run
// channel available at this altitude (neither workflow shares a journal/
// runID with the other's runs, or across its own runs). Implementation's
// initial escalation marker lives in the PR body so parking still posts only
// one human-facing comment; remediation then adopts it into this sticky state.
//
// It is ALSO the escalation-livelock breaker's (#716) self-heal snapshot: on
// an escalation, EscalatedHeadSHA/EscalatedBaseSHA record the PR's head/base
// at the moment escalation was recorded, so a later selection attempt
// (pr-select.go / gatherprcontext.go's escalationStillBlocks) can tell
// "genuinely still stuck" (current SHAs match) from "context changed since
// escalation — a sibling merge advanced base, or new commits landed" (SHAs
// differ), the latter re-enabling selection automatically without needing a
// human to clear the label.
type remediationState struct {
	// Cycles remains the total number of checkpoints recorded for operator
	// visibility and compatibility with existing sticky comments. It is not
	// used to enforce remediation budgets.
	Cycles int `json:"cycles"`
	// AttemptsByCause is the number of agentic remediation attempts admitted
	// for each independent cause. A cause only consumes its own allowance.
	AttemptsByCause remediationAttempts `json:"attemptsByCause,omitempty"`
	// LastDiffDigest is the content-addressed digest of the most recently
	// checkpointed cycle's `git diff base...HEAD` — compared against the
	// current cycle's digest to detect a no-progress repeat (#316's in-run
	// same-diff check, lifted to PR altitude per design doc §6 D5).
	LastDiffDigest string `json:"lastDiffDigest"`
	// HeadSHA / BaseSHA are the PR's head/base SHA at the moment THIS cycle
	// was recorded (every cycle, not only escalations) — the input the next
	// cycle's rebase-aware same-diff check reads back (#832). A byte-identical
	// LastDiffDigest only means "no progress" when the base has ALSO not moved
	// since: a clean rebase onto newer main legitimately reproduces the same
	// base...HEAD diff while advancing BaseSHA, which is progress toward
	// mergeability, not a stall. Empty on records written before #832 shipped,
	// in which case the check falls back to the digest-only behavior.
	HeadSHA string `json:"headSha,omitempty"`
	BaseSHA string `json:"baseSha,omitempty"`
	// Escalated marks this recorded state as an escalation event (goobers:
	// merge-escalated was applied) rather than an ordinary advancing cycle.
	Escalated bool `json:"escalated,omitempty"`
	// EscalatedReason is the human-readable cause (per-cause budget
	// exhaustion or a byte-identical repeat), carried so a later sticky-
	// comment edit can still render it without re-deriving it.
	EscalatedReason string `json:"escalatedReason,omitempty"`
	// EscalationOutcome and AttemptedCauses make the operational distinction
	// between failed repair, exhausted allowance, and policy exclusion durable.
	EscalationOutcome    remediationEscalationOutcome `json:"escalationOutcome,omitempty"`
	RemediationAttempted bool                         `json:"remediationAttempted"`
	AttemptedCauses      []remediationCause           `json:"attemptedCauses,omitempty"`
	// EscalatedHeadSHA / EscalatedBaseSHA are the PR's head/base SHA at the
	// moment of escalation, or the latest repeat fail — the self-heal
	// comparison snapshot (#716/#2378).
	EscalatedHeadSHA           string `json:"escalatedHeadSha,omitempty"`
	EscalatedBaseSHA           string `json:"escalatedBaseSha,omitempty"`
	SiblingOverlapContext      string `json:"siblingOverlapContext,omitempty"`
	StructuralCollisionContext string `json:"structuralCollisionContext,omitempty"`
	// EscalationCauses is the cause set observed on the cycle that escalated —
	// the cause CLASS escalationBaseAdvanceUnparks reads to decide whether a
	// base-branch advance alone may release the park. Empty on a forced or
	// policy-excluded escalation (no cause was ever observed, so no rebase
	// addresses it) and on records written before this field shipped;
	// EscalationGeneration separates those two cases.
	EscalationCauses []remediationCause `json:"escalationCauses,omitempty"`
	// EscalationGeneration counts how many times in a row this PR has been
	// parked at the SAME head SHA: 1 the first time a head is parked, +1 on
	// every re-escalation of that unchanged head. It is churn telemetry, never
	// a give-up threshold — no code path consults it to refuse work, so every
	// documented escape hatch stays reachable. It doubles as the marker that a
	// record was written by a binary that persists EscalationCauses (a zero
	// value means "cause class unknown", which keeps the pre-existing
	// unconditional base-advance self-heal).
	EscalationGeneration int `json:"escalationGeneration,omitempty"`
	// LastSeenCommentAt is the RFC3339 created-at of the newest issue-level PR
	// comment observed at the moment this cycle was recorded — the watermark
	// rebase-pr's human-comment detection (hasNewHumanCommentSince) compares
	// against so only a comment posted AFTER this cycle retriggers remediation.
	// Empty on records written before this shipped; hasNewHumanCommentSince
	// fails closed on an empty watermark so a fleet upgrade never retriggers
	// every PR that already carries a human comment.
	LastSeenCommentAt string `json:"lastSeenCommentAt,omitempty"`
}

// remediationStatePattern matches the machine-readable payload
// remediationStateComment appends to its posted comment.
var remediationStatePattern = regexp.MustCompile(`(?s)<!-- remediation-state: (.*?) -->`)
var implementationEscalationPattern = regexp.MustCompile(`(?s)<!-- implementation-escalation: (.*?) -->`)

type implementationEscalationState struct {
	DiffDigest string         `json:"diffDigest"`
	Reason     string         `json:"reason"`
	Cause      map[string]any `json:"cause,omitempty"`
}

func implementationEscalationMarker(state implementationEscalationState) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("marshal implementation escalation: %w", err)
	}
	return fmt.Sprintf("<!-- implementation-escalation: %s -->", data), nil
}

func withImplementationEscalationMarker(body string, state implementationEscalationState) (string, error) {
	marker, err := implementationEscalationMarker(state)
	if err != nil {
		return "", err
	}
	body = strings.TrimSpace(implementationEscalationPattern.ReplaceAllString(body, ""))
	if body == "" {
		return marker, nil
	}
	return body + "\n\n" + marker, nil
}

// remediationStateComment marshals s into the HTML-comment payload a
// checkpoint run posts as a PR comment.
func remediationStateComment(s remediationState) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal remediation-state payload: %w", err)
	}
	return fmt.Sprintf("<!-- remediation-state: %s -->", data), nil
}

// parseRemediationStateComment recovers the remediationState a prior
// checkpoint run embedded in a PR comment. Returns ok=false if body has no
// embedded payload — the normal "no checkpoint recorded yet" outcome for a
// PR's first pr-remediation cycle, not a parse error.
func parseRemediationStateComment(body string) (remediationState, bool) {
	m := remediationStatePattern.FindStringSubmatch(body)
	if m != nil {
		var s remediationState
		if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
			return remediationState{}, false
		}
		return s, true
	}
	m = implementationEscalationPattern.FindStringSubmatch(body)
	if m == nil {
		return remediationState{}, false
	}
	var state implementationEscalationState
	if err := json.Unmarshal([]byte(m[1]), &state); err != nil || state.DiffDigest == "" {
		return remediationState{}, false
	}
	// Seed the existing pre-agent same-diff guard. BaseSHA intentionally stays
	// empty because implementation records the content digest, not PR metadata;
	// remediationStalled's compatibility path compares the exact digest alone.
	return remediationState{
		LastDiffDigest:  state.DiffDigest,
		Escalated:       true,
		EscalatedReason: state.Reason,
	}, true
}

// renderRemediationComment builds the full sticky-comment body for state: a
// human-readable prose line (distinct for the escalated vs. ordinary-cycle
// case) followed by the embedded machine payload parseRemediationStateComment
// reads back. Escalated prose describes what ACTUALLY happens now (#716
// design item 4) — the PR is parked, not (as the old text falsely claimed)
// permanently excluded from ever being looked at again.
func renderRemediationComment(state remediationState) string {
	var prose string
	if state.Escalated {
		// State the exit that actually applies to THIS park's cause class,
		// rather than the blanket "head or base" the old text promised for
		// every escalation.
		unpark := "this PR's head changes"
		if escalationBaseAdvanceUnparks(state) {
			unpark = "this PR's head or base changes"
		}
		prose = fmt.Sprintf(
			"**pr-remediation escalated**\n\n%s. Parked until %s, or a human removes `%s`.",
			state.EscalatedReason, unpark, remediationEscalatedLabel,
		)
		if state.EscalationGeneration > 1 {
			prose += fmt.Sprintf(
				"\n\nThis is escalation %d for this unchanged head — remediation has already re-run and re-escalated it %d times.",
				state.EscalationGeneration, state.EscalationGeneration-1,
			)
		}
		if state.SiblingOverlapContext != "" {
			prose += "\n\n**Known sibling-overlap findings from merge-review:**\n" + state.SiblingOverlapContext
		}
		if state.StructuralCollisionContext != "" {
			prose += "\n\n**Same-function structural collision:**\n" + state.StructuralCollisionContext
		}
	} else {
		prose = fmt.Sprintf(
			"pr-remediation checkpoint: cycle %d, attempts by cause %s, diff digest `%s`.",
			state.Cycles, renderRemediationAttempts(state.AttemptsByCause), state.LastDiffDigest,
		)
	}
	payload, err := remediationStateComment(state)
	if err != nil {
		// Marshaling a plain struct of strings/ints/bools does not fail in
		// practice; if it somehow did, the prose alone is still a useful
		// comment — just without the machine-readable tail this run's own
		// state would otherwise carry forward.
		return prose
	}
	return prose + "\n\n" + payload
}

// postOrUpdateStickyComment posts state as a new PR comment, or — when
// existingCommentID names a comment already found on the thread (the sticky
// remediation-state comment a prior cycle posted) — edits that comment in
// place instead (#716 AC3: at most one escalation comment per PR per digest;
// repeated escalations/cycles edit the sticky comment rather than growing a
// new one every run).
func postOrUpdateStickyComment(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prNumber int, existingCommentID, body string) error {
	// Azure Boards work-item comments do not expose the issue-comment update
	// surface used by GitHub and Gitea. Re-post through the supported work-item
	// operation rather than failing a refusal cycle on an existing sticky ID.
	if _, ok := provider.(mergeReviewADOProvider); ok {
		_, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo,
			ID:         strconv.Itoa(prNumber),
			Comment:    body,
		})
		return err
	}
	if existingCommentID != "" {
		return provider.UpdateComment(ctx, repo, existingCommentID, body)
	}
	_, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(prNumber),
		Comment:    body,
	})
	return err
}

// A human may delete the sticky comment after ListComments returns. Recreate
// only that confirmed missing-comment race; every other provider error remains
// stage-fatal.
func postOrRecreateRemediationComment(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prNumber int, existingCommentID, body string) error {
	err := postOrUpdateStickyComment(ctx, provider, repo, prNumber, existingCommentID, body)
	if existingCommentID == "" || !providers.IsNotFoundError(err) {
		return err
	}
	return postOrUpdateStickyComment(ctx, provider, repo, prNumber, "", body)
}

type remediationCheckpointTransport interface {
	ListComments(context.Context) ([]providers.Comment, error)
	UpdateLabels(context.Context, []string, []string) error
	PutStickyComment(context.Context, string, string) error
}

type remediationCheckpointReader interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
}

type issueCommentCheckpointTransport struct {
	provider remediationProvider
	repo     providers.RepositoryRef
	number   int
}

func (t issueCommentCheckpointTransport) ListComments(ctx context.Context) ([]providers.Comment, error) {
	return t.provider.ListComments(ctx, t.repo, strconv.Itoa(t.number))
}

func (t issueCommentCheckpointTransport) UpdateLabels(ctx context.Context, add, remove []string) error {
	_, err := t.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   t.repo,
		ID:           strconv.Itoa(t.number),
		AddLabels:    add,
		RemoveLabels: remove,
	})
	return err
}

func (t issueCommentCheckpointTransport) PutStickyComment(ctx context.Context, existingCommentID, body string) error {
	return postOrRecreateRemediationComment(ctx, t.provider, t.repo, t.number, existingCommentID, body)
}

type threadCheckpointTransport struct {
	provider *providers.ADOProvider
	repo     providers.RepositoryRef
	pullID   string
}

func (t threadCheckpointTransport) ListComments(ctx context.Context) ([]providers.Comment, error) {
	return t.provider.ListPullRequestThreadComments(ctx, t.repo, t.pullID)
}

func (t threadCheckpointTransport) UpdateLabels(ctx context.Context, add, remove []string) error {
	if len(add) > 0 {
		if err := t.provider.AddPullRequestLabels(ctx, t.repo, t.pullID, add); err != nil {
			return err
		}
	}
	for _, label := range remove {
		if err := t.provider.RemovePullRequestLabel(ctx, t.repo, t.pullID, label); err != nil {
			return err
		}
	}
	return nil
}

func (t threadCheckpointTransport) PutStickyComment(ctx context.Context, existingCommentID, body string) error {
	return postOrRecreateRemediationThreadComment(ctx, t.provider, t.repo, t.pullID, existingCommentID, body)
}

type remediationCheckpointDecisionInput struct {
	Prior                      remediationState
	Causes                     []remediationCause
	Budgets                    remediationBudgets
	Digest                     string
	HeadSHA                    string
	BaseSHA                    string
	Watermark                  string
	Forced                     bool
	ForcedReason               string
	ForcedOutcome              remediationEscalationOutcome
	PolicyExcluded             bool
	StructuralCollisions       []structuralCollision
	StructuralCollisionContext string
}

type remediationCheckpointDecision struct {
	State            remediationState
	Escalation       remediationEscalation
	Escalated        bool
	HasObservedCause bool
}

func decideRemediationCheckpoint(in remediationCheckpointDecisionInput) remediationCheckpointDecision {
	stalled := remediationStalled(in.Prior, in.Digest, in.BaseSHA)
	exhaustedCause, exceeded := exhaustedRemediationCause(in.Prior.AttemptsByCause, in.Causes, in.Budgets)
	structuralCollision := len(in.StructuralCollisions) > 0
	cycles := in.Prior.Cycles + 1

	if exceeded || stalled || structuralCollision || in.Forced {
		attempted := in.Prior.AttemptsByCause
		if structuralCollision {
			for _, cause := range in.Causes {
				attempted.increment(cause)
			}
		}
		escalation := remediationEscalation{
			Outcome:         remediationOutcomeDidNotConverge,
			Attempted:       true,
			AttemptedCauses: attemptedRemediationCauses(attempted),
		}
		if in.Forced {
			escalation.Outcome = in.ForcedOutcome
		}
		if exceeded {
			escalation.Outcome = remediationOutcomeBudgetExhausted
			escalation.Reason = fmt.Sprintf(
				"remediation cause %q exhausted its budget after %d/%d attempts",
				exhaustedCause,
				in.Prior.AttemptsByCause.forCause(exhaustedCause),
				in.Budgets.forCause(exhaustedCause),
			)
		}
		if stalled {
			escalation.Reason = fmt.Sprintf("this cycle's diff is byte-identical to the immediately prior cycle's on the same base (digest %s) — an unchanged diff on an unchanged base cannot make progress", in.Digest)
		}
		if structuralCollision {
			collision := in.StructuralCollisions[0]
			escalation.Reason = fmt.Sprintf(
				"the rebase conflict falls inside `%s` in `%s`, which merged sibling PR #%d substantially restructured — retrying the same patch cannot resolve the required human/product reconciliation",
				collision.Function, collision.Path, collision.SiblingNumber,
			)
		}
		if in.Forced {
			escalation.Reason = in.ForcedReason
		}
		if in.PolicyExcluded {
			escalation.Outcome = remediationOutcomePolicyExcluded
			escalation.Attempted = false
			escalation.AttemptedCauses = nil
		}
		escalation.Generation = nextEscalationGeneration(in.Prior, in.HeadSHA)
		escalationCauses := in.Causes
		if in.Forced {
			escalationCauses = nil
		}
		return remediationCheckpointDecision{
			Escalated:  true,
			Escalation: escalation,
			State: remediationState{
				Cycles: cycles, AttemptsByCause: in.Prior.AttemptsByCause, LastDiffDigest: in.Digest,
				HeadSHA: in.HeadSHA, BaseSHA: in.BaseSHA,
				Escalated: true, EscalatedReason: escalation.Reason,
				EscalationOutcome: escalation.Outcome, RemediationAttempted: escalation.Attempted,
				AttemptedCauses:            escalation.AttemptedCauses,
				EscalatedHeadSHA:           in.HeadSHA,
				EscalatedBaseSHA:           in.BaseSHA,
				StructuralCollisionContext: in.StructuralCollisionContext,
				LastSeenCommentAt:          in.Watermark,
				EscalationCauses:           escalationCauses,
				EscalationGeneration:       escalation.Generation,
			},
		}
	}

	attempts := in.Prior.AttemptsByCause
	for _, cause := range in.Causes {
		attempts.increment(cause)
	}
	return remediationCheckpointDecision{
		HasObservedCause: len(in.Causes) > 0,
		State: remediationState{
			Cycles: cycles, AttemptsByCause: attempts, LastDiffDigest: in.Digest,
			HeadSHA: in.HeadSHA, BaseSHA: in.BaseSHA, LastSeenCommentAt: in.Watermark,
		},
	}
}

func remediationCheckpointMoot(stdout, stderr io.Writer, selectedNumber int) int {
	pln(stdout, "PR is no longer open (merged/closed since selection) — checkpoint moot, nothing to record")
	if err := writeCheckpointResult(stderr, false, selectedNumber, "", "", remediationEscalation{}); err != nil {
		return 1
	}
	return 0
}

// escalationStillBlocks reports whether pr's CURRENT goobers:merge-escalated
// label still blocks it from selection by merge-review's pr-select or
// pr-remediation's gather-pr-context (#716's core fix). A PR not currently
// carrying the label is never blocked by this check (false, nil) — this is
// the ordinary case for the vast majority of candidates and costs nothing.
//
// A PR that DOES carry the label is genuinely still stuck while its head is
// unchanged since the snapshot remediation-checkpoint recorded at the moment
// it escalated: nothing anyone did has moved the PR, so re-selecting it would
// just reproduce the same escalation. New commits (head SHA changed) always
// self-heal the park (AC2), without a human clearing the label by hand.
//
// A base-branch advance self-heals the park only when the recorded escalation
// cause is one a rebase onto the new base can plausibly cure —
// escalationBaseAdvanceUnparks. Treating ANY base advance as a self-heal made
// parking useless in a repo that merges dozens of PRs a day: every merge
// unparked every escalated PR, so a deterministically-doomed PR re-entered
// remediation within minutes and burned an agent session each time (the audit
// found 144 escalations across 48 PRs, one of them escalated 41 times in five
// days at an unchanged head).
//
// Base-advance detection compares the snapshot against the LIVE base-branch
// tip (provider.BranchTipSHA), NOT pr.BaseSHA. GitHub's pull_request.base.sha
// is a pinned commit that does not advance when the base branch does (#1052):
// keying the check off it made "a sibling merge advanced the base" a trigger
// that could never fire, so escalations were permanent-until-human and whole
// file-overlap clusters deadlocked. EscalatedBaseSHA is recorded as the live
// base tip at escalation time (see runRemediationCheckpoint), so this compares
// like-for-like.
//
// Fetches comments only for PRs that carry the label — a small, by-design
// subset — so this stays cheap for the common unlabeled case, and the extra
// ref lookup only for the parks a base advance can actually release.
func escalationStillBlocks(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	if !hasAnyLabel(pr.Labels, []string{remediationEscalatedLabel}) {
		return false, nil
	}
	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	state, _, found := latestRemediationState(rawComments)
	head, base := state.EscalatedHeadSHA, state.EscalatedBaseSHA
	if found && (!state.Escalated || head == "") {
		// The remediation-state comment is sticky and edited IN PLACE, so a
		// later non-escalation checkpoint overwrites the escalation snapshot
		// outright rather than sitting alongside it. Treating that as "no
		// snapshot" and failing closed parks the PR permanently: the head
		// comparison below is never reached, so moving the head — the exit
		// this label's own comment advertises — cannot release it.
		//
		// Every checkpoint records the PR's head/base at the moment it was
		// written, so fall back to those. That keeps the intended semantics
		// (an unchanged PR stays parked) while restoring the exit (#1855).
		head, base = state.HeadSHA, state.BaseSHA
	}
	if !found || head == "" {
		// Genuinely nothing to compare — a PR escalated before this payload
		// shipped, or a human applied the label by hand. Fail closed: still
		// blocks until a human clears the label.
		return true, nil
	}
	if head != pr.HeadSHA {
		return false, nil
	}
	if !escalationBaseAdvanceUnparks(state) {
		// No rebase addresses this park's cause, so the base tip carries no
		// information about it — skip the ref lookup entirely.
		return true, nil
	}
	liveBaseTip, err := provider.BranchTipSHA(ctx, repo, pr.Base)
	if err != nil {
		return false, err
	}
	if base != liveBaseTip {
		return false, nil
	}
	return true, nil
}

// escalationBaseAdvanceUnparks reports whether a base-branch advance ALONE may
// release state's park, i.e. whether a rebase onto the new base tip can
// plausibly cure the recorded escalation cause. A head change always releases
// a park regardless of this answer, so every escape hatch (push a commit,
// remove the label) stays reachable for every cause class.
//
// Rebase-curable: a rebase conflict, and a sibling-overlap rejection whose
// resolution IS the sibling landing on the base branch. Not rebase-curable: a
// substantive reviewer rejection, failing CI, and a human comment — those are
// properties of the PR's own content, so re-running remediation against an
// unchanged head reproduces the same escalation no matter how far the base has
// moved. Same for infrastructure-failure and policy-excluded outcomes (the PR
// was never evaluated on its merits, and no base advance changes that) and for
// a structural collision, which is escalated precisely because retrying the
// patch cannot resolve it.
func escalationBaseAdvanceUnparks(state remediationState) bool {
	if !state.Escalated || state.EscalationGeneration == 0 {
		// Not an escalation record (the #1855 fallback reads an ordinary
		// cycle's head/base), or an escalation recorded before the cause class
		// was persisted: the cause is unknowable, so keep the pre-existing
		// unconditional base-advance self-heal rather than retro-parking PRs.
		// Records age out on the next checkpoint, which rewrites the sticky
		// comment in place.
		return true
	}
	switch state.EscalationOutcome {
	case remediationOutcomeInfrastructure, remediationOutcomePolicyExcluded:
		return false
	}
	if state.StructuralCollisionContext != "" {
		return false
	}
	if len(state.EscalationCauses) == 0 {
		// A forced escalation (a reviewer verdict of fail, or a repeated
		// implementer no-op) observed no remediation cause at all.
		return false
	}
	for _, cause := range state.EscalationCauses {
		if !baseAdvanceCuresRemediationCause(cause) {
			return false
		}
	}
	return true
}

func baseAdvanceCuresRemediationCause(cause remediationCause) bool {
	switch cause {
	case remediationCauseConflict, remediationCauseSiblingOverlap:
		return true
	default:
		return false
	}
}

// nextEscalationGeneration returns the EscalationGeneration to record when
// headSHA is parked on top of prior: 1 for the first park of a given head, +1
// for each re-escalation of that same unchanged head. An escalation record
// written before the counter shipped counts as the first park of its head, so
// an upgraded fleet reports the churn it can still prove rather than restarting
// every parked PR's count at 1.
func nextEscalationGeneration(prior remediationState, headSHA string) int {
	if !prior.Escalated || prior.EscalatedHeadSHA == "" || prior.EscalatedHeadSHA != headSHA {
		return 1
	}
	generation := prior.EscalationGeneration
	if generation < 1 {
		generation = 1
	}
	return generation + 1
}

// refreshEscalationSnapshotAfterRepeatFail closes the one-shot self-heal
// window after merge-review re-evaluates an already-escalated PR. Without this,
// a base advance stays different from the original snapshot forever, making an
// unchanged PR eligible on every subsequent poll (#2378).
//
// Its caller only reaches here on a verdict of fail, which is a rejection of
// the PR's content: no rebase cures that, so the refreshed snapshot drops any
// rebase-curable cause the earlier park recorded — a further base advance is
// not new evidence about a PR a reviewer just re-failed at this same head.
func refreshEscalationSnapshotAfterRepeatFail(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, comments []providers.Comment) error {
	state, commentID, found := latestRemediationState(comments)
	if !found || commentID == "" {
		return nil
	}
	liveBaseTip, err := provider.BranchTipSHA(ctx, repo, pr.Base)
	if err != nil {
		return err
	}
	if state.Escalated && state.EscalatedHeadSHA == pr.HeadSHA && state.EscalatedBaseSHA == liveBaseTip {
		return nil
	}
	state.EscalationGeneration = nextEscalationGeneration(state, pr.HeadSHA)
	state.Escalated = true
	state.EscalatedHeadSHA = pr.HeadSHA
	state.EscalatedBaseSHA = liveBaseTip
	state.EscalationCauses = nil
	return provider.UpdateComment(ctx, repo, commentID, renderRemediationComment(state))
}

// latestRemediationState scans comments (oldest first, ListComments' own
// order) for the LAST one carrying an embedded remediation-state payload —
// only the most recently recorded cycle/escalation is still actionable —
// and also returns that comment's ID, so a caller can edit it in place
// (postOrUpdateStickyComment) rather than posting a new one. found is false
// if no comment in the thread carries a payload (the PR's first
// pr-remediation cycle), not an error.
func latestRemediationState(comments []providers.Comment) (state remediationState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := parseRemediationStateComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return remediationState{}, "", false
}

func latestRemediationStateForPR(body string, comments []providers.Comment) (state remediationState, commentID string, found bool) {
	if s, ok := parseRemediationStateComment(body); ok {
		state, found = s, true
	}
	if s, id, ok := latestRemediationState(comments); ok {
		return s, id, true
	}
	return state, "", found
}

// latestCommentTimestamp returns the RFC3339 (UTC) created-at of the newest
// comment that carries one, or "" when none do. This is the human-comment
// watermark a checkpoint records in remediationState.LastSeenCommentAt so the
// next rebase-pr cycle only retriggers on a comment posted strictly after it.
func latestCommentTimestamp(comments []providers.Comment) string {
	var newest time.Time
	found := false
	for _, c := range comments {
		if c.CreatedAt == nil {
			continue
		}
		if !found || c.CreatedAt.After(newest) {
			newest = *c.CreatedAt
			found = true
		}
	}
	if !found {
		return ""
	}
	return newest.UTC().Format(time.RFC3339)
}

// runRemediationCheckpoint implements `goobers remediation-checkpoint`
// (issue #364): lifts in-run repass control and same-diff escalation (#316,
// LastDiffDigest) to PR altitude (design doc §6 D4/D5). Per-cause budgets are
// supplied by the workflow DSL (issue #953). Meant to run as
// pr-remediation's last stage each cycle, immediately after whichever
// stage(s) push the remediated branch (#363) — it re-checks-out the PR's
// own branch itself (this stage gets its own fresh worktree; an earlier
// stage's checkout does not survive to here, same reason gather-pr-context
// and rebase-pr each do their own), reads the PR's most recently recorded
// cause-attempt counts + diff digest back from a sticky PR comment, compares
// this cycle's actual diff against it, and either escalates
// (goobers:merge-escalated, clearing needs-remediation so the machine stops
// selecting it) when one cause exhausts its own budget or on a byte-identical
// repeat, or records the advanced state for next cycle.
const remediationCheckpointHelp = "Usage: goobers remediation-checkpoint [--budget N] [--escalate <reason> [--escalation-outcome <outcome>]] [path]\n\n" +
	"Re-checkout the PR's own branch (this stage gets its own fresh\n" +
	"worktree), read pr-remediation's durable per-cause attempt counters + last\n" +
	"diff digest back from a sticky PR comment, compare this cycle's\n" +
	"actual diff (git diff base...HEAD) against it, and either\n" +
	"escalate (goobers:merge-escalated, clearing needs-remediation) when\n" +
	"the active cause exhausts its DSL-declared budget or on a byte-identical\n" +
	"repeat, or record the advanced\n" +
	"state as a new sticky comment. Requires selectedNumber (inputsFrom\n" +
	"gather-pr-context's selectedNumber output), remediationCauses, and the\n" +
	"five per-cause budget inputs (humanCommentBudget defaults to 2 when\n" +
	"undeclared). --budget overrides every declared cause\n" +
	"for standalone diagnostics. --escalation-outcome classifies a forced\n" +
	"--escalate as did-not-converge (the default), budget-exhausted, or infrastructure-failure.\n" +
	"Escalations persist a machine-readable `escalationOutcome`\n" +
	"(`did-not-converge`, `budget-exhausted`, `policy-excluded`, or `infrastructure-failure`), whether\n" +
	"repair was attempted, and the attempted causes in both the sticky comment\n" +
	"and stage result. Exit codes: 0 = checkpoint\n" +
	"recorded (escalated or not — both are normal outcomes), 1 = business\n" +
	"error, 2 = usage/IO error.\n"

func runRemediationCheckpoint(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("remediation-checkpoint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "remediation-checkpoint")
	budgetOverride := fs.Int("budget", 0, "override every DSL-declared per-cause budget (standalone diagnostics)")
	// --escalate is the reviewer-verdict=fail path (design doc §4 D2: "a
	// fundamentally wrong approach is not burned on remediation budget"), not
	// a loop-control outcome: escalate unconditionally with the caller's
	// reason, skipping the budget and same-diff checks entirely. Issue #392.
	escalateReason := fs.String("escalate", "", "escalate unconditionally with this reason, skipping the D4/D5 checks")
	escalationOutcome := fs.String(
		"escalation-outcome",
		string(remediationOutcomeDidNotConverge),
		"machine-readable outcome for --escalate (did-not-converge, budget-exhausted, or infrastructure-failure)",
	)
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
	forcedOutcome := remediationEscalationOutcome(*escalationOutcome)
	switch forcedOutcome {
	case remediationOutcomeDidNotConverge, remediationOutcomeBudgetExhausted, remediationOutcomeInfrastructure:
	default:
		pf(stderr, "error: --escalation-outcome must be %q, %q, or %q\n", remediationOutcomeDidNotConverge, remediationOutcomeBudgetExhausted, remediationOutcomeInfrastructure)
		return 2
	}
	if *escalateReason == "" && forcedOutcome != remediationOutcomeDidNotConverge {
		pf(stderr, "error: --escalation-outcome requires --escalate\n")
		return 2
	}
	root := providerStageRoot(pathArg)

	var selectedNumber int
	if selectedNumberStr := providerInput("selectedNumber", ""); selectedNumberStr != "" {
		n, err := strconv.Atoi(selectedNumberStr)
		if err != nil {
			pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
			return 1
		}
		selectedNumber = n
	} else {
		// Ledger fallback (#392): in --escalate mode this stage runs after the
		// agentic chain, where Task.InputsFrom can no longer reach
		// gather-pr-context's selectedNumber — implement and local-ci each
		// became the upstream in turn. The run's own PR claim is the durable
		// answer. Still an error if there is no claim either: escalating
		// without knowing WHICH PR would label an arbitrary one.
		n, ok, err := claimedPullRequestNumber(root)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		if !ok {
			pf(stderr, "error: selectedNumber is required (inputsFrom gather-pr-context's selectedNumber output, or a PR claim held by this run)\n")
			return 1
		}
		selectedNumber = n
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Azure DevOps reaches a NEW branch that talks to the small ADO primitive
	// surface (PR threads + PR labels + PollPullRequest), mirroring the
	// merge-review ADO stages (runPRSelectADO, runGatherSiblingContextADO). It is
	// entered before any github:pr:write token is resolved — ADO carries its own
	// org-scoped auth (remediation-wiring-plan §3.5). The forced-escalation flags
	// (--escalate/--escalation-outcome/--budget) are already parsed and validated
	// above, so they flow in; every other input the ADO body reads itself.
	if repo.Provider == providers.ProviderADO {
		return runRemediationCheckpointADO(root, repo, selectedNumber, *escalateReason, forcedOutcome, *budgetOverride, stdout, stderr)
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// repo:push is not for writing here — it's the same credential
	// checkoutExistingBranch uses to fetch the PR's branch (see the
	// re-checkout comment below).
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, err := remediationStageProvider(root, repo, token, true)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	transport := issueCommentCheckpointTransport{provider: provider, repo: repo, number: selectedNumber}

	base := providerInput("base", providerBaseBranch())
	headPrefix := providerInput("headPrefix", providerBranchNamespace())
	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix, SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "")
	}
	var current *providers.PullRequestSummary
	for i := range prs {
		if prs[i].Number == selectedNumber {
			current = &prs[i]
			break
		}
	}
	if current == nil {
		// Halt, don't continue: there is no longer a PR to remediate, so
		// spending an agentic session on it would be pure waste (#392).
		return remediationCheckpointMoot(stdout, stderr, selectedNumber)
	}
	// The list may come from the scheduler tick's shared snapshot. Refresh the
	// selected PR before acting on state that can already be stale.
	refreshed, err := provider.GetPullRequest(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
	}
	if refreshed.State != "open" || refreshed.Merged {
		return remediationCheckpointMoot(stdout, stderr, selectedNumber)
	}
	current = &refreshed

	// Re-checkout the PR's own branch: this stage gets its OWN fresh
	// worktree (internal/runner's buildEnvelope keys worktree continuity on
	// the run's shared branch, not on whatever an earlier stage locally
	// checked out — #133/#363's own re-checkout for the same reason), so
	// gather-pr-context's or rebase-pr's checkout does not survive to here.
	// Without this, diffDigest would diff against whatever this fresh
	// worktree defaulted to (the run's own untouched base checkout), not
	// the PR's actual just-pushed content.
	// ...UNLESS this worktree is already on that branch, which since #392 is
	// the normal case: gather-pr-context rebinds the run's workspace branch to
	// the PR's head, so the runner provisions this stage on it directly.
	//
	// Re-checking-out anyway would be actively destructive rather than merely
	// redundant. checkoutExistingBranch is `git checkout -B <branch>
	// FETCH_HEAD` — a hard reset to the REMOTE tip — and on the
	// substantive-findings path rebase-pr deliberately does NOT push its
	// rebase (rebasepr.go's `!conflict && !hasSubstantiveFindings` guard), so
	// that rebase exists only as a local commit on this very branch. Resetting
	// here would discard it, hand `implement` an un-rebased tree, and leave the
	// PR still behind its base after a successful remediation — looping every
	// such PR until the D4 budget escalated it.
	//
	// A forced escalation (--escalate) skips all of it: the caller already has
	// a terminal reason, so no digest is consulted to reach the decision. Doing
	// the fetch anyway would let a transient git/network failure exit non-zero
	// BEFORE the labelling below — leaving goobers:merge-escalated unapplied
	// and goobers:needs-remediation still set, so the PR is re-selected and the
	// reviewer re-rejects the same approach, which is exactly what §4 D2's
	// escalate-immediately rule exists to prevent.
	conflicted, err := strconv.ParseBool(providerInput("conflict", "false"))
	if err != nil {
		pf(stderr, "error: invalid conflict input: %v\n", err)
		return 1
	}
	attemptedHeadSHA := providerInput("attemptedHeadSha", "")
	// A rebase-pr cycle whose only detected cause(s) the declared `remediate`
	// policy excludes (#941/PRR-6) escalates immediately here, exactly like
	// an explicit --escalate: no agent ever ran on this cycle, so budget/
	// no-progress checks would just delay the same inevitable outcome.
	// *escalateReason (the reviewer-fail path, §4 D2) wins if somehow both
	// are set — it is the more specific, caller-supplied terminal reason.
	policyExcluded, err := strconv.ParseBool(providerInput("policyExcluded", "false"))
	if err != nil {
		pf(stderr, "error: invalid policyExcluded input: %v\n", err)
		return 1
	}
	escalateReasonValue := *escalateReason
	if escalateReasonValue == "" {
		if policyExcluded {
			escalateReasonValue = providerInput("policyExcludedReason", "declared remediation policy excludes the only detected cause(s)")
		}
	}
	forced := escalateReasonValue != ""
	if *budgetOverride < 0 {
		pf(stderr, "error: --budget must not be negative\n")
		return 1
	}
	var causes []remediationCause
	var budgets remediationBudgets
	if !forced {
		rawCauses := providerInput("remediationCauses", "")
		if strings.TrimSpace(rawCauses) != "" {
			causes, err = parseRemediationCauses(rawCauses)
			if err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			budgets, err = declaredRemediationBudgets(*budgetOverride)
			if err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
		}
	}
	if !forced && sequencingOnlyCheckpointWait(current.Labels, causes) {
		if hasAnyLabel(current.Labels, []string{needsRemediationLabel}) {
			if err := transport.UpdateLabels(ctx, nil, []string{needsRemediationLabel}); err != nil {
				return failProviderStage(stderr, fmt.Sprintf("clear needs-remediation label from sequencing-only PR #%d", selectedNumber), err, "")
			}
		}
		if err := writeCheckpointResult(stderr, false, selectedNumber, current.Head, current.HeadSHA, remediationEscalation{}); err != nil {
			return 1
		}
		pf(stdout, "PR #%d is blocked only on sibling sequencing — waiting without consuming remediation budget\n", selectedNumber)
		return 0
	}
	var (
		digest               string
		structuralCollisions []structuralCollision
		rawComments          []providers.Comment
	)
	forceHeadRefresh := false
	forceBaseRefresh := false
	stableCheckpoint := false
	const maxCheckpointRefreshes = 3
	for refreshAttempt := 0; refreshAttempt < maxCheckpointRefreshes; refreshAttempt++ {
		attemptMatchesLiveHead := false
		if conflicted && !forced && attemptedHeadSHA != "" {
			liveCurrent, err := provider.GetPullRequest(ctx, repo, strconv.Itoa(selectedNumber))
			if err != nil {
				return failProviderStage(stderr, fmt.Sprintf("get live state for PR #%d", selectedNumber), err, "")
			}
			if liveCurrent.State != "open" || liveCurrent.Merged {
				return remediationCheckpointMoot(stdout, stderr, selectedNumber)
			}
			current = &liveCurrent
			attemptMatchesLiveHead = attemptedHeadSHA == current.HeadSHA
		}

		digest = ""
		if !forced {
			onBranch, err := currentBranchIs(".", current.Head)
			if err != nil {
				pf(stderr, "error: resolve current branch for PR #%d: %v\n", selectedNumber, err)
				return 1
			}
			refreshBranch := forceHeadRefresh || !onBranch
			if onBranch && conflicted && attemptedHeadSHA != "" {
				localHeadSHA, err := resolveHead(".")
				if err != nil {
					pf(stderr, "error: resolve current head for PR #%d: %v\n", selectedNumber, err)
					return 1
				}
				// A conflicted rebase was aborted, so there is no local rebase to
				// preserve when a concurrent push makes this checkout stale.
				refreshBranch = refreshBranch || localHeadSHA != current.HeadSHA
			}
			if refreshBranch {
				fetchedSHA, err := checkoutExistingBranch(".", current.Head, pushToken)
				if err != nil {
					pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selectedNumber, current.Head, err)
					return 1
				}
				current.HeadSHA = fetchedSHA
				if conflicted && attemptedHeadSHA != "" {
					attemptMatchesLiveHead = attemptedHeadSHA == fetchedSHA
				}
			}
			if forceBaseRefresh {
				if _, err := fetchExistingBranch(".", current.Base, pushToken); err != nil {
					pf(stderr, "error: refresh PR #%d's base branch %q: %v\n", selectedNumber, current.Base, err)
					return 1
				}
			}
			digest, err = diffDigest(".", current.BaseSHA)
			if err != nil {
				pf(stderr, "error: compute diff digest for PR #%d: %v\n", selectedNumber, err)
				return 1
			}
		}

		structuralCollisions = nil
		if conflicted && !forced && attemptMatchesLiveHead {
			conflictLocations, err := decodeConflictLocations(providerInput("conflictLocations", ""))
			if err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			structuralCollisions, err = findStructuralCollisions(
				ctx, provider, repo, *current, base, headPrefix, conflictLocations,
				".", pushToken, providerInput("rebaseBaseSha", ""),
			)
			if err != nil {
				return failProviderStage(stderr, fmt.Sprintf("detect same-function structural collision for PR #%d", selectedNumber), err, "")
			}
		}

		rawComments, err = transport.ListComments(ctx)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selectedNumber), err, "")
		}
		// Keep this read independent from current: assigning through the value
		// current points at would pair newly read metadata with the old digest.
		lateCurrent, err := provider.GetPullRequest(ctx, repo, strconv.Itoa(selectedNumber))
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
		}
		if lateCurrent.State != "open" || lateCurrent.Merged {
			return remediationCheckpointMoot(stdout, stderr, selectedNumber)
		}
		headChanged := lateCurrent.Head != current.Head || lateCurrent.HeadSHA != current.HeadSHA
		baseChanged := lateCurrent.Base != current.Base || lateCurrent.BaseSHA != current.BaseSHA
		current = &lateCurrent
		if !forced && (headChanged || baseChanged) {
			forceHeadRefresh = headChanged
			forceBaseRefresh = baseChanged
			continue
		}
		stableCheckpoint = true
		break
	}
	if !stableCheckpoint {
		return failProviderStage(
			stderr,
			fmt.Sprintf("stabilize pull request #%d", selectedNumber),
			fmt.Errorf("head or base changed during %d consecutive checkpoint reads", maxCheckpointRefreshes),
			"",
		)
	}
	if !forced && len(causes) > 0 {
		signature := remediationNoopSignature{
			HeadSHA: current.HeadSHA,
			Causes:  normalizeRemediationCauses(providerInput("remediationCauses", "")),
		}
		l := layoutFor(root)
		noOpRecord, err := remediationNoopRecordForSignature(l, selectedNumber, signature)
		if err != nil {
			pf(stderr, "error: inspect remediation no-op guard: %v\n", err)
			return 1
		}
		gaggle := l.Gaggle()
		if gaggle == "" {
			gaggle = providerGaggle()
		}
		key := remediationNoopKey(gaggle, selectedNumber)
		if noOpRecord.Parked && !hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
			if err := clearRemediationNoopRecord(l, key); err != nil {
				pf(stderr, "error: reset operator-cleared remediation no-op guard: %v\n", err)
				return 1
			}
			noOpRecord = remediationNoopRecord{}
		}
		if noOpRecord.Attempts >= remediationNoopLimit {
			if err := markRemediationNoopParked(l, key); err != nil {
				pf(stderr, "error: park remediation no-op guard: %v\n", err)
				return 1
			}
			escalateReasonValue = fmt.Sprintf(
				"the implementer reported no-work %d consecutive times for unchanged head %s and remediation cause(s) %s",
				noOpRecord.Attempts, signature.HeadSHA, signature.Causes,
			)
			forced = true
		}
	}
	// Latest comment carrying an embedded payload wins, same rationale as
	// gather-pr-context's verdict scan: only the most recently recorded
	// checkpoint state is still actionable. Its comment ID (if any) is the
	// sticky comment this cycle edits in place (#716 AC3), rather than
	// posting a new one.
	prior, priorCommentID, _ := latestRemediationStateForPR(current.Body, rawComments)

	// #1808: the escalation comment advertises three unpark paths, one of which
	// is "a human removes goobers:merge-escalated". That path did not work. The
	// repass count lives in this checkpoint's comment payload, not in the label,
	// so clearing the label let the PR back into the loop with its counter still
	// over budget — and the very next cycle re-escalated for the same reason. On
	// PR #1729 that round trip took under six minutes, with the count stuck at
	// 12/10 throughout.
	//
	// It is also the only exit an operator can take directly: the other two
	// require actually modifying the PR, so for anything blocked on something no
	// agent can fix, the documented escape hatch was inert.
	//
	// An operator clearing the escalation is an explicit request for a fresh
	// attempt, so drop the whole prior record rather than only the counter.
	// Keeping LastDiffDigest would immediately re-escalate an unchanged diff via
	// the stall check — a different reason, the same dead end. priorCommentID is
	// deliberately preserved so this cycle still edits the sticky comment in
	// place instead of posting a new one.
	if prior.Escalated && !hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
		pf(stdout, "PR #%d: escalation cleared by an operator — resetting remediation budget (was %d cycles, attempts %s)\n",
			selectedNumber, prior.Cycles, renderRemediationAttempts(prior.AttemptsByCause))
		prior = remediationState{}
	}

	// Record the human-comment watermark this cycle sees. The refresh loop lists
	// rawComments unconditionally (even on forced escalations), so the newest
	// timestamp is always available; when the thread carries no timestamped
	// comment, carry the prior watermark forward rather than dropping it (which
	// would fail closed and never retrigger on human-comment again).
	watermark := latestCommentTimestamp(rawComments)
	if watermark == "" {
		watermark = prior.LastSeenCommentAt
	}

	decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior: prior, Causes: causes, Budgets: budgets, Digest: digest,
		HeadSHA: current.HeadSHA, BaseSHA: current.BaseSHA, Watermark: watermark,
		Forced: forced, ForcedReason: escalateReasonValue, ForcedOutcome: forcedOutcome,
		PolicyExcluded: policyExcluded, StructuralCollisions: structuralCollisions,
		StructuralCollisionContext: renderStructuralCollisionContext(selectedNumber, structuralCollisions),
	})
	if decision.Escalated {
		var overlaps []siblingOverlapFinding
		if !forced && len(structuralCollisions) == 0 {
			overlaps, err = knownSiblingOverlapFindings(
				ctx, provider, repo, selectedNumber, base, headPrefix, prs,
				time.Now().UTC().Add(-siblingOverlapLookback),
			)
			if err != nil {
				return failProviderStage(stderr, fmt.Sprintf("load sibling-overlap findings for PR #%d", selectedNumber), err, "")
			}
		}
		// Record the LIVE base-branch tip, not current.BaseSHA (GitHub's
		// pinned pull_request.base.sha): escalationStillBlocks compares this
		// snapshot against the live tip to detect a base advance, and
		// pull_request.base.sha never moves when the base branch does, so
		// pinning it here would make the self-heal comparison always match
		// and the escalation permanent-until-human (#1052).
		var escalatedBaseTip string
		escalatedBaseTip, err = provider.BranchTipSHA(ctx, repo, base)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("resolve base branch %q tip for PR #%d", base, selectedNumber), err, "")
		}
		decision.State.EscalatedBaseSHA = escalatedBaseTip
		decision.State.SiblingOverlapContext = renderSiblingOverlapContext(overlaps)
		if err := transport.UpdateLabels(ctx, []string{remediationEscalatedLabel}, []string{needsRemediationLabel}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("escalate PR #%d", selectedNumber), err, "")
		}
		if err := transport.PutStickyComment(ctx, priorCommentID, renderRemediationComment(decision.State)); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("record escalation comment on PR #%d", selectedNumber), err, "")
		}
		if err := writeCheckpointResult(stderr, false, selectedNumber, current.Head, current.HeadSHA, decision.Escalation); err != nil {
			return 1
		}
		pf(stdout, "escalated PR #%d (escalation %d for head %s): %s\n", selectedNumber, decision.Escalation.Generation, current.HeadSHA, decision.Escalation.Reason)
		return 0
	}

	// Clear the label before replacing its escalation snapshot. Otherwise a
	// failed label removal leaves a non-escalated state that fails closed.
	if hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
		if err := transport.UpdateLabels(ctx, nil, []string{remediationEscalatedLabel}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("clear self-healed escalation label from PR #%d", selectedNumber), err, "")
		}
	}
	if err := transport.PutStickyComment(ctx, priorCommentID, renderRemediationComment(decision.State)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("record checkpoint state on PR #%d", selectedNumber), err, "")
	}
	if err := writeCheckpointResult(stderr, decision.HasObservedCause, selectedNumber, current.Head, current.HeadSHA, remediationEscalation{}); err != nil {
		return 1
	}

	if !decision.HasObservedCause {
		pf(
			stdout,
			"recorded no-cause checkpoint for PR #%d: cycle %d, counters unchanged, digest %s; remediation halted without consuming an allowance\n",
			selectedNumber, decision.State.Cycles, digest,
		)
		return 0
	}
	pf(
		stdout,
		"recorded checkpoint for PR #%d: cycle %d, attempts by cause %s, digest %s\n",
		selectedNumber, decision.State.Cycles, renderRemediationAttempts(decision.State.AttemptsByCause), digest,
	)
	return 0
}

// runRemediationCheckpointADO is the Azure DevOps counterpart of
// runRemediationCheckpoint's loop-control body (remediation-wiring-plan §3.5).
// ADO gaps every surface the GitHub path leans on for this stage, so the loop is
// rebuilt on the small ADO primitives Phase 1 added, reached only when the
// routed repo is ADO — every GitHub call site stays byte-identical:
//
//   - The sticky remediation-state comment — the durable cross-run channel that
//     carries the per-cause attempt counters, the last diff digest, and (the
//     single most important checkpoint output for the loop) the pre-remediation
//     head SHA push-remediated leases against — lives on a PR THREAD:
//     posted/updated via PostPullRequestThreadComment /
//     UpdatePullRequestThreadComment and read back via
//     ListPullRequestThreadComments. ADO has no PR-comment transport, so the
//     GitHub ListComments/UpdateComment/UpdateWorkItem(comment) trio — all of
//     which address WORK ITEMS on ADO (the PR-as-work-item wrong-object hazard,
//     §0.5) — is never reached.
//   - Escalation (add goobers:merge-escalated, clear goobers:needs-remediation)
//     and the self-heal clear (remove goobers:merge-escalated) go through the
//     native PR-LABEL surface (AddPullRequestLabels / RemovePullRequestLabel),
//     NEVER UpdateWorkItem(ID: PR#) (the GitHub path's :1121 / :1151), which on
//     ADO mutates the unrelated work item that shares the PR's numeric id.
//
// Two ADO provider facts shape it: GetPullRequest → PollPullRequest does NOT
// populate ADO PR labels (only ListPullRequests maps them), so the PR's live
// head/base/state come from GetPullRequest while its labels come from the
// ListPullRequests snapshot; and there is no per-ref check surface, none of
// which this loop-control stage needs.
//
// Deliberately narrower than the GitHub body for the first working ADO loop (§6
// scope cut): the escalation self-heal snapshot (escalationStillBlocks /
// base-advance unpark, which needs BranchTipSHA + the sibling scan) is
// RECORD-ONLY here — pr-select's ADO branch does not consult it (§6) — and the
// structural-collision, sibling-overlap-context, no-op-guard, and multi-read
// stabilization paths are omitted. A forced escalation (a reviewer verdict of
// fail via --escalate, or a declared-policy exclusion) skips the git checkout +
// diff digest exactly as on GitHub.
func runRemediationCheckpointADO(
	root string,
	repo providers.RepositoryRef,
	selectedNumber int,
	escalateReason string,
	forcedOutcome remediationEscalationOutcome,
	budgetOverride int,
	stdout, stderr io.Writer,
) int {
	provider, err := newProviderForStageAs[*providers.ADOProvider](root, repo, false)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pullID := strconv.Itoa(selectedNumber)
	transport := threadCheckpointTransport{provider: provider, repo: repo, pullID: pullID}
	return runTransportedRemediationCheckpoint(
		root, repo, selectedNumber, escalateReason, forcedOutcome, budgetOverride,
		provider, transport, stdout, stderr,
	)
}

func runTransportedRemediationCheckpoint(
	root string,
	repo providers.RepositoryRef,
	selectedNumber int,
	escalateReason string,
	forcedOutcome remediationEscalationOutcome,
	budgetOverride int,
	provider remediationCheckpointReader,
	transport remediationCheckpointTransport,
	stdout, stderr io.Writer,
) int {
	base := providerInput("base", providerBaseBranch())
	headPrefix := providerInput("headPrefix", providerBranchNamespace())
	pullID := strconv.Itoa(selectedNumber)
	ctx, cancel := providerCommandContext()
	defer cancel()

	// A forced escalation is not attributable to an observed remediation cause,
	// consults no digest, and touches no git — decide it before resolving the
	// push credential or checking out the branch, exactly like the GitHub body.
	// *escalateReason (the reviewer-fail path) wins if both are set.
	policyExcluded, err := strconv.ParseBool(providerInput("policyExcluded", "false"))
	if err != nil {
		pf(stderr, "error: invalid policyExcluded input: %v\n", err)
		return 1
	}
	escalateReasonValue := escalateReason
	if escalateReasonValue == "" && policyExcluded {
		escalateReasonValue = providerInput("policyExcludedReason", "declared remediation policy excludes the only detected cause(s)")
	}
	forced := escalateReasonValue != ""
	if budgetOverride < 0 {
		pf(stderr, "error: --budget must not be negative\n")
		return 1
	}

	// ADO's ListPullRequests is the ONLY surface that maps native PR labels
	// (GetPullRequest → PollPullRequest leaves them empty), so the escalated/
	// needs-remediation label reads below must come from it. GetPullRequest then
	// supplies the authoritative live head/base/state (the snapshot may be stale
	// and leaves State empty on ADO); the labels are carried over.
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix, SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "")
	}
	var listedLabels []string
	found := false
	for i := range prs {
		if prs[i].Number == selectedNumber {
			listedLabels = prs[i].Labels
			found = true
			break
		}
	}
	if !found {
		// ADO's active-PR list excludes completed/abandoned PRs, so a vanished
		// selection is moot — nothing to remediate (#392).
		return remediationCheckpointMoot(stdout, stderr, selectedNumber)
	}
	refreshed, err := provider.GetPullRequest(ctx, repo, pullID)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
	}
	if refreshed.State != "open" || refreshed.Merged {
		return remediationCheckpointMoot(stdout, stderr, selectedNumber)
	}
	refreshed.Labels = listedLabels
	current := &refreshed

	var causes []remediationCause
	var budgets remediationBudgets
	if !forced {
		rawCauses := providerInput("remediationCauses", "")
		if strings.TrimSpace(rawCauses) != "" {
			causes, err = parseRemediationCauses(rawCauses)
			if err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			budgets, err = declaredRemediationBudgets(budgetOverride)
			if err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
		}
	}
	if !forced && sequencingOnlyCheckpointWait(current.Labels, causes) {
		if hasAnyLabel(current.Labels, []string{needsRemediationLabel}) {
			if err := transport.UpdateLabels(ctx, nil, []string{needsRemediationLabel}); err != nil {
				return failProviderStage(stderr, fmt.Sprintf("clear needs-remediation label from sequencing-only PR #%d", selectedNumber), err, "")
			}
		}
		if err := writeCheckpointResult(stderr, false, selectedNumber, current.Head, current.HeadSHA, remediationEscalation{}); err != nil {
			return 1
		}
		pf(stdout, "PR #%d is blocked only on sibling sequencing — waiting without consuming remediation budget\n", selectedNumber)
		return 0
	}

	// This cycle's diff digest against the PR's base — the same-diff stall
	// signal (§6 D5). Only on the non-forced path, and only after the stage's own
	// re-checkout of the PR branch (this stage gets a fresh worktree). The git
	// operations are provider-neutral and work on ADO with the repo:push
	// credential; a forced escalation skips them entirely, so its repo:push is
	// never required.
	digest := ""
	if !forced {
		pushToken, tokErr := providerToken(capability.RepoPush)
		if tokErr != nil {
			pf(stderr, "error: %v\n", tokErr)
			return 1
		}
		onBranch, brErr := currentBranchIs(".", current.Head)
		if brErr != nil {
			pf(stderr, "error: resolve current branch for PR #%d: %v\n", selectedNumber, brErr)
			return 1
		}
		if !onBranch {
			fetchedSHA, coErr := checkoutExistingBranch(".", current.Head, pushToken)
			if coErr != nil {
				pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selectedNumber, current.Head, coErr)
				return 1
			}
			current.HeadSHA = fetchedSHA
		}
		digest, err = diffDigest(".", current.BaseSHA)
		if err != nil {
			pf(stderr, "error: compute diff digest for PR #%d: %v\n", selectedNumber, err)
			return 1
		}
	}

	rawComments, err := transport.ListComments(ctx)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list thread comments on PR #%d", selectedNumber), err, "")
	}
	prior, priorCommentID, _ := latestRemediationStateForPR(current.Body, rawComments)

	// An operator clearing goobers:merge-escalated is an explicit request for a
	// fresh attempt: the repass counter lives in the sticky comment, not the
	// label, so drop the whole prior record (keeping priorCommentID so this cycle
	// still edits the sticky thread in place rather than posting a new one).
	if prior.Escalated && !hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
		pf(stdout, "PR #%d: escalation cleared by an operator — resetting remediation budget (was %d cycles, attempts %s)\n",
			selectedNumber, prior.Cycles, renderRemediationAttempts(prior.AttemptsByCause))
		prior = remediationState{}
	}

	watermark := latestCommentTimestamp(rawComments)
	if watermark == "" {
		watermark = prior.LastSeenCommentAt
	}

	decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior: prior, Causes: causes, Budgets: budgets, Digest: digest,
		HeadSHA: current.HeadSHA, BaseSHA: current.BaseSHA, Watermark: watermark,
		Forced: forced, ForcedReason: escalateReasonValue, ForcedOutcome: forcedOutcome,
		PolicyExcluded: policyExcluded,
	})
	if decision.Escalated {
		// Escalate via the native PR-label surface — NEVER UpdateWorkItem(PR#).
		// Add merge-escalated first, then clear needs-remediation, so a failure
		// between the two leaves the PR blocked (escalated) rather than
		// selectable-but-unmarked.
		if err := transport.UpdateLabels(ctx, []string{remediationEscalatedLabel}, []string{needsRemediationLabel}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("escalate PR #%d", selectedNumber), err, "")
		}
		if err := transport.PutStickyComment(ctx, priorCommentID, renderRemediationComment(decision.State)); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("record escalation comment on PR #%d", selectedNumber), err, "")
		}
		if err := writeCheckpointResult(stderr, false, selectedNumber, current.Head, current.HeadSHA, decision.Escalation); err != nil {
			return 1
		}
		pf(stdout, "escalated PR #%d (escalation %d for head %s): %s\n", selectedNumber, decision.Escalation.Generation, current.HeadSHA, decision.Escalation.Reason)
		return 0
	}

	// A self-healed escalation (head/base advanced since the park): clear the
	// label before replacing the escalation snapshot, so a failed clear leaves
	// the PR parked rather than in a non-escalated state that fails closed.
	if hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
		if err := transport.UpdateLabels(ctx, nil, []string{remediationEscalatedLabel}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("clear self-healed escalation label from PR #%d", selectedNumber), err, "")
		}
	}
	if err := transport.PutStickyComment(ctx, priorCommentID, renderRemediationComment(decision.State)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("record checkpoint state on PR #%d", selectedNumber), err, "")
	}
	if err := writeCheckpointResult(stderr, decision.HasObservedCause, selectedNumber, current.Head, current.HeadSHA, remediationEscalation{}); err != nil {
		return 1
	}
	if !decision.HasObservedCause {
		pf(
			stdout,
			"recorded no-cause checkpoint for PR #%d: cycle %d, counters unchanged, digest %s; remediation halted without consuming an allowance\n",
			selectedNumber, decision.State.Cycles, digest,
		)
		return 0
	}
	pf(
		stdout,
		"recorded checkpoint for PR #%d: cycle %d, attempts by cause %s, digest %s\n",
		selectedNumber, decision.State.Cycles, renderRemediationAttempts(decision.State.AttemptsByCause), digest,
	)
	return 0
}

// postOrUpdateStickyThreadComment is the ADO analog of postOrUpdateStickyComment:
// it edits the existing sticky remediation-state thread comment in place when one
// was found (existingCommentID is the composite "<pullID>/<threadId>/<commentId>"
// a prior List/Post returned), otherwise opens a new PR thread. It never touches
// wit/workitems (the PR-as-work-item wrong-object hazard the GitHub
// UpdateWorkItem(comment) fallback would trip on ADO).
func postOrUpdateStickyThreadComment(ctx context.Context, provider *providers.ADOProvider, repo providers.RepositoryRef, pullID, existingCommentID, body string) error {
	if existingCommentID != "" {
		return provider.UpdatePullRequestThreadComment(ctx, repo, existingCommentID, body)
	}
	_, err := provider.PostPullRequestThreadComment(ctx, repo, pullID, body)
	return err
}

// postOrRecreateRemediationThreadComment is postOrRecreateRemediationComment for
// ADO threads: a human may delete the sticky thread comment after the list read,
// so a confirmed not-found on the in-place update falls back to posting a fresh
// thread. Every other provider error stays stage-fatal.
func postOrRecreateRemediationThreadComment(ctx context.Context, provider *providers.ADOProvider, repo providers.RepositoryRef, pullID, existingCommentID, body string) error {
	err := postOrUpdateStickyThreadComment(ctx, provider, repo, pullID, existingCommentID, body)
	if existingCommentID == "" || !providers.IsNotFoundError(err) {
		return err
	}
	return postOrUpdateStickyThreadComment(ctx, provider, repo, pullID, "", body)
}

func parseRemediationCauses(raw string) ([]remediationCause, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("remediationCauses is required")
	}

	seen := make(map[remediationCause]bool, len(remediationCauseOrder))
	for _, value := range strings.Split(raw, ",") {
		cause := remediationCause(strings.TrimSpace(value))
		switch cause {
		case remediationCauseConflict, remediationCauseSubstantive, remediationCauseFailingCI, remediationCauseSiblingOverlap, remediationCauseHumanComment:
		default:
			return nil, fmt.Errorf("unknown remediation cause %q", value)
		}
		seen[cause] = true
	}
	causes := make([]remediationCause, 0, len(seen))
	for _, cause := range remediationCauseOrder {
		if seen[cause] {
			causes = append(causes, cause)
		}
	}
	return causes, nil
}

// defaultHumanCommentBudget is the per-cycle allowance for the human-comment
// cause when humanCommentBudget is undeclared. It is a DEFAULT rather than a
// required input (unlike the four legacy budgets): declaredRemediationBudgets
// runs whenever any cause fires, so requiring it would fail every already-
// deployed workflow the moment it upgraded to a binary that reads it.
const defaultHumanCommentBudget = 2

func declaredRemediationBudgets(override int) (remediationBudgets, error) {
	if override > 0 {
		return remediationBudgets{
			Conflict: override, Substantive: override,
			FailingCI: override, SiblingOverlap: override,
			HumanComment: override,
		}, nil
	}
	var budgets remediationBudgets
	values := []struct {
		input  string
		target *int
	}{
		{"conflictBudget", &budgets.Conflict},
		{"substantiveBudget", &budgets.Substantive},
		{"failingCIBudget", &budgets.FailingCI},
		{"siblingOverlapBudget", &budgets.SiblingOverlap},
	}
	for _, value := range values {
		raw := providerInput(value.input, "")
		budget, err := strconv.Atoi(raw)
		if err != nil || budget <= 0 {
			return remediationBudgets{}, fmt.Errorf("%s must be a positive integer, got %q", value.input, raw)
		}
		*value.target = budget
	}
	// humanCommentBudget is optional for backward compatibility: an empty input
	// falls back to defaultHumanCommentBudget so a legacy workflow that predates
	// the cause keeps working, while a non-empty but invalid value is still a
	// hard error (a typo must not silently pick up the default).
	if raw := providerInput("humanCommentBudget", ""); raw == "" {
		budgets.HumanComment = defaultHumanCommentBudget
	} else {
		budget, err := strconv.Atoi(raw)
		if err != nil || budget <= 0 {
			return remediationBudgets{}, fmt.Errorf("humanCommentBudget must be a positive integer, got %q", raw)
		}
		budgets.HumanComment = budget
	}
	return budgets, nil
}

func exhaustedRemediationCause(attempts remediationAttempts, causes []remediationCause, budgets remediationBudgets) (remediationCause, bool) {
	for _, cause := range causes {
		if attempts.forCause(cause) >= budgets.forCause(cause) {
			return cause, true
		}
	}
	return "", false
}

func sequencingOnlyCheckpointWait(labels []string, causes []remediationCause) bool {
	if !hasAnyLabel(labels, []string{blockedOnSiblingLabel}) {
		return false
	}
	for _, cause := range causes {
		if cause != remediationCauseSiblingOverlap {
			return false
		}
	}
	return true
}

func renderRemediationAttempts(attempts remediationAttempts) string {
	parts := make([]string, 0, len(remediationCauseOrder))
	for _, cause := range remediationCauseOrder {
		parts = append(parts, fmt.Sprintf("%s=%d", cause, attempts.forCause(cause)))
	}
	return strings.Join(parts, ", ")
}

type remediationEscalation struct {
	Outcome         remediationEscalationOutcome
	Attempted       bool
	AttemptedCauses []remediationCause
	Reason          string
	// Generation is the escalation's EscalationGeneration — how many times this
	// unchanged head has now been parked. Emitted so a workflow or telemetry
	// query can see re-escalation churn without reconstructing it from PR
	// comments. Zero on a non-escalating checkpoint.
	Generation int
}

func attemptedRemediationCauses(attempts remediationAttempts) []remediationCause {
	var causes []remediationCause
	for _, cause := range remediationCauseOrder {
		if attempts.forCause(cause) > 0 {
			causes = append(causes, cause)
		}
	}
	return causes
}

func knownSiblingOverlapFindings(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	selectedNumber int,
	base, headPrefix string,
	openPRs []providers.PullRequestSummary,
	closedSince time.Time,
) ([]siblingOverlapFinding, error) {
	closedPRs, err := provider.ListRecentlyClosedPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	}, closedSince)
	if err != nil {
		return nil, fmt.Errorf("list recently closed pull requests: %w", err)
	}

	candidates := make(map[int]providers.PullRequestSummary, len(openPRs)+len(closedPRs))
	for _, candidate := range openPRs {
		candidates[candidate.Number] = candidate
	}
	for _, candidate := range closedPRs {
		// The terminal query runs second, so it is the current-state winner
		// when a sibling closes between the two snapshots.
		candidates[candidate.Number] = candidate
	}
	referencePattern := regexp.MustCompile(fmt.Sprintf(
		`(?i)(?:(?:\bPR\b|\bpull request\b)\s*#?\s*%d\b|#%d\b)`,
		selectedNumber, selectedNumber,
	))
	numbers := make([]int, 0, len(candidates))
	for number := range candidates {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	var overlaps []siblingOverlapFinding
	for _, number := range numbers {
		candidate := candidates[number]
		if candidate.Number == selectedNumber {
			continue
		}
		comments, err := provider.ListComments(ctx, repo, strconv.Itoa(candidate.Number))
		if err != nil {
			return nil, fmt.Errorf("list comments on sibling PR #%d: %w", candidate.Number, err)
		}
		for i := len(comments) - 1; i >= 0; i-- {
			verdict, ok := parseVerdictComment(comments[i].Body)
			if !ok {
				continue
			}
			matched := false
			for _, finding := range verdict.Findings {
				if !siblingFindingReferencesPR(finding, selectedNumber, referencePattern) {
					continue
				}
				overlaps = append(overlaps, siblingOverlapFinding{
					Number: candidate.Number, State: pullRequestState(candidate),
					Message: finding.Message, Location: finding.Location,
				})
				matched = true
			}
			if matched {
				break
			}
		}
	}
	sort.Slice(overlaps, func(i, j int) bool {
		if overlaps[i].Number != overlaps[j].Number {
			return overlaps[i].Number < overlaps[j].Number
		}
		if overlaps[i].Message != overlaps[j].Message {
			return overlaps[i].Message < overlaps[j].Message
		}
		return overlaps[i].Location < overlaps[j].Location
	})
	return overlaps, nil
}

func siblingFindingReferencesPR(finding apiv1.Finding, selectedNumber int, referencePattern *regexp.Regexp) bool {
	if finding.Class == apiv1.FindingCrossPRBlocked {
		for _, blocker := range finding.BlockingPRs {
			if blocker == selectedNumber {
				return true
			}
		}
		if len(finding.BlockingPRs) == 0 {
			return referencePattern.MatchString(finding.Message) || referencePattern.MatchString(finding.Location)
		}
		return false
	}
	if !finding.Class.RequiresCodeChange() {
		return false
	}
	return referencePattern.MatchString(finding.Message) || referencePattern.MatchString(finding.Location)
}

func pullRequestState(pr providers.PullRequestSummary) string {
	if pr.Merged {
		return "merged"
	}
	if strings.EqualFold(pr.State, "closed") {
		return "closed"
	}
	return "open"
}

func renderSiblingOverlapContext(overlaps []siblingOverlapFinding) string {
	var lines []string
	for _, overlap := range overlaps {
		line := fmt.Sprintf("- Sibling PR #%d is **%s**: %s", overlap.Number, overlap.State, overlap.Message)
		if overlap.Location != "" {
			line += " (" + overlap.Location + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// writeCheckpointResult emits this stage's routing output (issue #392).
//
// continueRemediation is what checkpoint-gate branches on: "true" means this
// cycle may spend the agentic chain on the PR, "false" means it must not —
// because the checkpoint escalated it (a cause budget exhausted, a no-progress
// repeat, or a caller-forced reviewer "fail"), or because the PR is no
// longer open. It is a "true"/"false" STRING for the same reason
// gather-pr-context stringifies its own booleans: only string-valued
// top-level result-file keys survive into a downstream stage's GOOBERS_INPUT_*
// env var.
//
// selectedNumber/head/headSha are echoed forward because a gate never updates
// Task.InputsFrom's upstream-Outputs chain (rebase-pr's writeRebaseResult doc
// establishes the convention) — push-remediated sits two hops past
// checkpoint-gate, so anything it needs from here must be re-emitted here.
// headSha in particular is the PR's remote tip BEFORE this cycle pushes
// anything, which is exactly the non-tautological --force-with-lease
// expectation push-remediated requires.
//
// The default matches the shipped workflow so standalone invocations preserve
// the same routing output contract.
func writeCheckpointResult(stderr io.Writer, continueRemediation bool, selectedNumber int, head, headSHA string, escalation remediationEscalation) error {
	resultFile := providerInput("resultFile", "checkpoint-result.json")
	attemptedCauses := make([]string, 0, len(escalation.AttemptedCauses))
	for _, cause := range escalation.AttemptedCauses {
		attemptedCauses = append(attemptedCauses, string(cause))
	}
	data, err := json.Marshal(map[string]string{
		"continueRemediation":  strconv.FormatBool(continueRemediation),
		"selectedNumber":       strconv.Itoa(selectedNumber),
		"head":                 head,
		"headSha":              headSHA,
		"escalationOutcome":    string(escalation.Outcome),
		"remediationAttempted": strconv.FormatBool(escalation.Attempted),
		"attemptedCauses":      strings.Join(attemptedCauses, ","),
		"escalationReason":     escalation.Reason,
		"escalationGeneration": strconv.Itoa(escalation.Generation),
	})
	if err != nil {
		pf(stderr, "error: marshal checkpoint result: %v\n", err)
		return err
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return err
	}
	return nil
}

// currentBranchIs reports whether dir's checked-out branch is already branch.
// A detached HEAD (rev-parse prints "HEAD") is never a match, so the caller
// falls back to an explicit checkout.
func currentBranchIs(dir, branch string) (bool, error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, fmt.Errorf("git rev-parse --abbrev-ref HEAD: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return false, fmt.Errorf("git rev-parse --abbrev-ref HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)) == branch, nil
}

// remediationStalled reports whether this cycle is a genuine no-progress
// repeat that should trip the same-diff escalation (design doc §6 D5): the
// cycle's diff is byte-identical to the prior recorded cycle's AND the base
// has not advanced since.
//
// The base clause is #832's fix. A clean rebase onto newer main legitimately
// reproduces the same `base...HEAD` diff while advancing BaseSHA — that is
// what a clean rebase IS — and being current with main is progress toward
// mergeability, not a stall, so a byte-identical diff after a base advance
// must NOT escalate. Only an identical diff on the SAME base is genuinely
// stuck. Uses base rather than head deliberately: an identical-content
// re-push advances the head SHA without making any progress, so head
// movement alone must not suppress escalation. When prior.BaseSHA is empty
// (a state recorded before #832 shipped, or the PR's first cycle), the base
// clause is inert and behavior falls back to the original digest-only check.
func remediationStalled(prior remediationState, digest, currentBaseSHA string) bool {
	sameDiff := prior.LastDiffDigest != "" && prior.LastDiffDigest == digest
	rebasedSincePrior := prior.BaseSHA != "" && prior.BaseSHA != currentBaseSHA
	return sameDiff && !rebasedSincePrior
}

// diffDigest returns the hex-encoded sha256 digest of `git diff
// baseSHA...HEAD` at dir — the same content-addressing idea
// internal/worktree.Worktree.Diff + internal/runner's recordReviewerDiff use
// for the in-run same-diff check (#316), computed directly here since
// pr-remediation's stages (like gather-pr-context before it,
// checkoutExistingBranch/isBehindBase) shell out to git directly rather
// than going through the runner-internal Worktree type.
func diffDigest(dir, baseSHA string) (string, error) {
	if baseSHA == "" {
		return "", fmt.Errorf("PR has no recorded base SHA")
	}
	cmd := exec.Command("git", "diff", baseSHA+"...HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git diff %s...HEAD: %w: %s", baseSHA, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git diff %s...HEAD: %w", baseSHA, err)
	}
	sum := sha256.Sum256(out)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
