package remediationselect

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// RemediationEscalatedLabel is the label that parks a PR pending a human.
const RemediationEscalatedLabel = providers.LabelMergeEscalated

// RemediationCause names why a PR needs remediation.
type RemediationCause string

// The remediation causes, in the order the checkpoint spends their budgets.
const (
	RemediationCauseConflict       RemediationCause = RemediateCauseConflict
	RemediationCauseSubstantive    RemediationCause = RemediateCauseSubstantive
	RemediationCauseFailingCI      RemediationCause = RemediateCauseFailingCI
	RemediationCauseSiblingOverlap RemediationCause = RemediateCauseSiblingOverlap
	RemediationCauseHumanComment   RemediationCause = RemediateCauseHumanComment
)

// RemediationEscalationOutcome is why a PR was parked for a human.
type RemediationEscalationOutcome string

// The remediation escalation outcomes.
const (
	RemediationOutcomeDidNotConverge  RemediationEscalationOutcome = "did-not-converge"
	RemediationOutcomeBudgetExhausted RemediationEscalationOutcome = "budget-exhausted"
	RemediationOutcomePolicyExcluded  RemediationEscalationOutcome = "policy-excluded"
	RemediationOutcomeInfrastructure  RemediationEscalationOutcome = "infrastructure-failure"
	// remediationOutcomeExternallyBlocked marks a park taken BEFORE any agent
	// ran, because every detected cause is external to the PR's own diff
	// (#2701): sibling sequencing merge-review already recorded, or required CI
	// that is red on the base branch itself. An implementer confined to this
	// PR's worktree cannot change any of them, so running the agentic chain
	// reproduces the identical diff and escalates anyway — one full remediation
	// pass spent to reach an outcome that was knowable here.
	RemediationOutcomeExternallyBlocked RemediationEscalationOutcome = "externally-blocked"
)

// RemediationCauseOrder is the stable order causes are considered and reported in.
var RemediationCauseOrder = []RemediationCause{
	RemediationCauseConflict,
	RemediationCauseSubstantive,
	RemediationCauseFailingCI,
	RemediationCauseSiblingOverlap,
	RemediationCauseHumanComment,
}

// RemediationAttempts counts the attempts spent per cause.
type RemediationAttempts struct {
	Conflict       int `json:"conflict,omitempty"`
	Substantive    int `json:"substantive,omitempty"`
	FailingCI      int `json:"failing-ci,omitempty"`
	SiblingOverlap int `json:"sibling-overlap,omitempty"`
	HumanComment   int `json:"human-comment,omitempty"`
}

// ForCause returns the attempts spent on cause.
func (a RemediationAttempts) ForCause(cause RemediationCause) int {
	switch cause {
	case RemediationCauseConflict:
		return a.Conflict
	case RemediationCauseSubstantive:
		return a.Substantive
	case RemediationCauseFailingCI:
		return a.FailingCI
	case RemediationCauseSiblingOverlap:
		return a.SiblingOverlap
	case RemediationCauseHumanComment:
		return a.HumanComment
	default:
		return 0
	}
}

// Increment charges one attempt to cause.
func (a *RemediationAttempts) Increment(cause RemediationCause) {
	switch cause {
	case RemediationCauseConflict:
		a.Conflict++
	case RemediationCauseSubstantive:
		a.Substantive++
	case RemediationCauseFailingCI:
		a.FailingCI++
	case RemediationCauseSiblingOverlap:
		a.SiblingOverlap++
	case RemediationCauseHumanComment:
		a.HumanComment++
	}
}

// Refund returns one attempt of cause's allowance, never below zero — a
// voided cycle's charge is given back (#5588).
func (a *RemediationAttempts) Refund(cause RemediationCause) {
	switch cause {
	case RemediationCauseConflict:
		a.Conflict = max(a.Conflict-1, 0)
	case RemediationCauseSubstantive:
		a.Substantive = max(a.Substantive-1, 0)
	case RemediationCauseFailingCI:
		a.FailingCI = max(a.FailingCI-1, 0)
	case RemediationCauseSiblingOverlap:
		a.SiblingOverlap = max(a.SiblingOverlap-1, 0)
	case RemediationCauseHumanComment:
		a.HumanComment = max(a.HumanComment-1, 0)
	}
}

// RemediationBudgets is the attempt allowance per cause.
type RemediationBudgets struct {
	Conflict       int
	Substantive    int
	FailingCI      int
	SiblingOverlap int
	HumanComment   int
}

// ForCause returns the allowance for cause.
func (b RemediationBudgets) ForCause(cause RemediationCause) int {
	switch cause {
	case RemediationCauseConflict:
		return b.Conflict
	case RemediationCauseSubstantive:
		return b.Substantive
	case RemediationCauseFailingCI:
		return b.FailingCI
	case RemediationCauseSiblingOverlap:
		return b.SiblingOverlap
	case RemediationCauseHumanComment:
		return b.HumanComment
	default:
		return 0
	}
}

// SiblingOverlapFinding is one sibling PR a PR overlaps with, as recorded in the checkpoint.
type SiblingOverlapFinding struct {
	Number   int    `json:"number"`
	State    string `json:"state"`
	Message  string `json:"message"`
	Location string `json:"location,omitempty"`
}

// RemediationState is pr-remediation's OWN durable per-PR loop-control
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
type RemediationState struct {
	// Cycles remains the total number of checkpoints recorded for operator
	// visibility and compatibility with existing sticky comments. It is not
	// used to enforce remediation budgets.
	Cycles int `json:"cycles"`
	// AttemptsByCause is the number of agentic remediation attempts admitted
	// for each independent cause. A cause only consumes its own allowance.
	AttemptsByCause RemediationAttempts `json:"attemptsByCause,omitempty"`
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
	EscalationOutcome    RemediationEscalationOutcome `json:"escalationOutcome,omitempty"`
	RemediationAttempted bool                         `json:"remediationAttempted"`
	AttemptedCauses      []RemediationCause           `json:"attemptedCauses,omitempty"`
	// EscalatedHeadSHA / EscalatedBaseSHA are the PR's head/base SHA at the
	// moment of escalation, or the latest repeat fail — the self-heal
	// comparison snapshot (#716/#2378).
	EscalatedHeadSHA           string `json:"escalatedHeadSha,omitempty"`
	EscalatedBaseSHA           string `json:"escalatedBaseSha,omitempty"`
	SiblingOverlapContext      string `json:"siblingOverlapContext,omitempty"`
	StructuralCollisionContext string `json:"structuralCollisionContext,omitempty"`
	// EscalationCauses is the cause set observed on the cycle that escalated —
	// the cause CLASS escalationBaseAdvanceUnparks reads to decide whether a
	// base-branch advance alone may release the park. Empty on a
	// policy-excluded escalation (the PR was never evaluated on its merits) and
	// on records written before this field shipped; EscalationGeneration
	// separates those two cases, and AttemptedCauses is the fallback for a
	// record written by a binary that had not yet learned to populate this
	// field on the forced path (#4089).
	EscalationCauses []RemediationCause `json:"escalationCauses,omitempty"`
	// EscalationCausesCleared records that EscalationCauses was emptied ON
	// PURPOSE — refreshEscalationSnapshotAfterRepeatFail drops the cause set
	// when a reviewer re-fails an unchanged head, because no rebase cures a
	// rejection of the PR's content (#2378). It is the marker that keeps the
	// #4089 AttemptedCauses fallback from resurrecting a window that was
	// deliberately closed.
	EscalationCausesCleared bool `json:"escalationCausesCleared,omitempty"`
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
	// NoopGuardRepark marks a record written by the gather-pr-context
	// unchanged-digest guard's re-park path (recordGatherPRContextDigestNoop),
	// as opposed to an ordinary remediation-checkpoint escalation. That guard
	// requires TWO goobers:merge-escalated clears to release a park — the
	// first cheaply no-ops (#716/#2378) — so the single-clear exit text
	// renderRemediationComment otherwise renders is wrong for this record
	// (#4174). It never gates any budget or escalation decision itself.
	NoopGuardRepark bool `json:"noopGuardRepark,omitempty"`
	// RunID and ChargedCauses make an advancing cycle's charge provisional
	// (#5588/#5598): the checkpoint charges AttemptsByCause and records
	// LastDiffDigest BEFORE the agent runs, so the run that recorded the
	// cycle is named here along with exactly what it charged. Empty on
	// escalations and on records written before this shipped.
	RunID         string             `json:"runId,omitempty"`
	ChargedCauses []RemediationCause `json:"chargedCauses,omitempty"`
	// InfrastructureVoided is set by the daemon's failed-terminal handler
	// (voidRemediationChargeForRun) when RunID ended on an infrastructure
	// fault — the cycle's fix was never evaluated. The next checkpoint refunds
	// ChargedCauses and skips the same-diff comparison against this cycle
	// instead of reading it as a failed or stalled attempt.
	InfrastructureVoided bool `json:"infrastructureVoided,omitempty"`
	// InfrastructureFailures counts consecutive cycles voided that way before
	// this one. It bounds the refund: a PR whose every cycle dies on
	// infrastructure escalates infrastructure-failure after
	// remediationInfrastructureAllowance of them rather than looping forever.
	InfrastructureFailures int `json:"infrastructureFailures,omitempty"`
	// Superseded is set by the daemon's completed-terminal handler
	// (settleSupersededRun, #6019) when RunID ended because the pull request
	// moved under it — someone pushed to it while the run was working — before
	// the run published anything. The cycle's fix was built for a revision
	// the PR no longer has, so like an infrastructure void its ChargedCauses
	// are refunded and its digest is dropped from the same-diff comparison.
	// Unlike one, it is not a strike toward the infrastructure allowance: a
	// person's push says nothing about the substrate.
	Superseded bool `json:"superseded,omitempty"`
	// SupersededCycles counts consecutive cycles refunded that way before this
	// one. It bounds the refund the way InfrastructureFailures bounds an
	// infrastructure void: a PR whose head keeps moving under every cycle (an
	// active author, another bot, an auto-update) escalates after
	// remediationSupersededAllowance of them instead of spending an agent
	// session per cycle forever, invisible to every other counter.
	SupersededCycles int `json:"supersededCycles,omitempty"`
	// OperatorRetry records the last audited `goobers pr retry` that reset
	// this state.
	OperatorRetry *OperatorRetryRecord `json:"operatorRetry,omitempty"`
}

// RemediationStatePattern matches the machine-readable payload
// remediationStateComment appends to its posted comment.
var RemediationStatePattern = regexp.MustCompile(`(?s)<!-- remediation-state: (.*?) -->`)

// ImplementationEscalationPattern matches the machine-readable implementation-escalation payload.
var ImplementationEscalationPattern = regexp.MustCompile(`(?s)<!-- implementation-escalation: (.*?) -->`)

// ImplementationEscalationState is the implementation-escalation payload.
type ImplementationEscalationState struct {
	DiffDigest string         `json:"diffDigest"`
	Reason     string         `json:"reason"`
	Cause      map[string]any `json:"cause,omitempty"`
}

// ParseRemediationStateComment recovers the remediationState a prior
// checkpoint run embedded in a PR comment. Returns ok=false if body has no
// embedded payload — the normal "no checkpoint recorded yet" outcome for a
// PR's first pr-remediation cycle, not a parse error.
func ParseRemediationStateComment(body string) (RemediationState, bool) {
	m := RemediationStatePattern.FindStringSubmatch(body)
	if m != nil {
		var s RemediationState
		if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
			return RemediationState{}, false
		}
		return s, true
	}
	m = ImplementationEscalationPattern.FindStringSubmatch(body)
	if m == nil {
		return RemediationState{}, false
	}
	var state ImplementationEscalationState
	if err := json.Unmarshal([]byte(m[1]), &state); err != nil || state.DiffDigest == "" {
		return RemediationState{}, false
	}
	// Seed the existing pre-agent same-diff guard. BaseSHA intentionally stays
	// empty because implementation records the content digest, not PR metadata;
	// remediationStalled's compatibility path compares the exact digest alone.
	return RemediationState{
		LastDiffDigest:  state.DiffDigest,
		Escalated:       true,
		EscalatedReason: state.Reason,
	}, true
}

// EscalationStillBlocks reports whether pr's CURRENT goobers:merge-escalated
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
// EscalationBaseAdvanceUnparks. Treating ANY base advance as a self-heal made
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
func EscalationStillBlocks(ctx context.Context, provider Provider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	if !HasAnyLabel(pr.Labels, []string{RemediationEscalatedLabel}) {
		return false, nil
	}
	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	state, _, found := LatestRemediationState(rawComments)
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
		// pr-remediation is not the only writer of this label: merge-review
		// applies it too, on a verdict of fail (applyverdict.go's verdictLabel
		// contract, design doc §4 D2), and that path never writes a
		// remediation-state comment at all — its record rides the verdict-json
		// payload on the merge-review-status comment instead. Failing closed
		// here would park every merge-review escalation permanently, which is
		// the same dead end #1855 fixed one marker over: the head comparison
		// below is never reached, so pushing a commit cannot release a park
		// whose own comment advertises exactly that exit.
		//
		// The verdict pins its head/base for SHA-pinning already, so read the
		// snapshot from there. The escalating verdict is a fail — a rejection
		// of the PR's content — so the synthesized record carries no
		// rebase-curable cause and escalationBaseAdvanceUnparks keeps
		// returning false for it, exactly as for a forced escalation.
		if vHead, vBase, ok := LatestMergeReviewEscalationPins(rawComments); ok {
			state = RemediationState{Escalated: true, EscalationGeneration: 1}
			head, base = vHead, vBase
		} else if verdict, ok := LatestMergeReviewVerdict(rawComments); ok {
			// A merge-review escalation does not always leave a fail verdict on
			// the PR it escalates. The cluster election writes its fail on the
			// CLUSTER WINNER's thread and labels the deferring siblings, so a
			// sibling ends up carrying goobers:merge-escalated whose only
			// verdict-json payloads are its own needs-changes or pass rows
			// (#4038). Those siblings hit the fail-closed return below and were
			// parked with NO exit at all: pr-remediation excludes escalated PRs
			// upstream, so the head could never move, so the head comparison
			// that releases the park was unreachable. Live on 2026-08-31 that
			// was four of the five open bot PRs — three of them with a verdict
			// of pass — and it starved the lane to `no work` on every tick.
			//
			// The verdict pins the head/base it was computed against whatever
			// the decision, so use it as the snapshot, and derive the cause
			// class from whether it faults the PR itself rather than assuming
			// one:
			state = RemediationState{Escalated: true, EscalationGeneration: 1}
			if EscalationIsOrderingOnly(verdict, pr.Number) {
				// The verdict does not fault this PR's own content, so the
				// escalation cannot be about this PR — it is the cluster's
				// ordering. That is the cause baseAdvanceCuresRemediationCause
				// calls rebase-curable, so a base advance releases it: the
				// sibling it was waiting behind has landed.
				state.EscalationCauses = []RemediationCause{RemediationCauseSiblingOverlap}
			}
			// Otherwise the causes stay empty and escalationBaseAdvanceUnparks
			// keeps returning false, so a park recorded against the PR's own
			// content still survives a base advance and is released only by a
			// head change — the same asymmetry the fail path above relies on.
			head, base = verdict.HeadSHA, verdict.BaseSHA
		}
	}
	if head == "" {
		// Genuinely nothing to compare — a PR escalated before any payload
		// shipped, or a human applied the label by hand. Fail closed: still
		// blocks until a human clears the label.
		return true, nil
	}
	if head != pr.HeadSHA {
		return false, nil
	}
	if !EscalationBaseAdvanceUnparks(state) {
		// No rebase addresses this park's cause, so the base tip carries no
		// information about it — skip the ref lookup entirely.
		return true, nil
	}
	liveBaseTip, err := provider.BranchTipSHA(ctx, repo, pr.Base)
	if err != nil {
		return false, err
	}
	if base != liveBaseTip {
		// #2702: the base moved, but for an ordering park that is only news if
		// the ordering itself moved. Ask before spending a cycle.
		unchanged, err := SiblingOrderingUnchanged(ctx, provider, repo, state, rawComments)
		if err != nil {
			return false, err
		}
		return unchanged, nil
	}
	return true, nil
}

// SiblingOrderingUnchanged reports whether an ordering-only escalation's
// recorded blockers are ALL still blocking, which makes a base advance
// uninformative about this park (#2702).
//
// The cadence loop it closes: a PR escalated behind a sibling awaiting a human
// ordering decision is parked with cause sibling-overlap, which
// BaseAdvanceCuresRemediationCause calls rebase-curable — correctly, since the
// canonical cure IS the sibling landing, and that advances the base. But on an
// active repository the base advances constantly for unrelated reasons, and
// each advance re-opened eligibility for a PR whose blockers had not moved.
// Every tick then ran the full pipeline — context, sibling analysis,
// checkpoint, respond-to-findings, implement — to reach the same terminal
// answer. Live, one PR was re-attempted daily for five consecutive days.
//
// This narrows ONLY the sibling-overlap case, and only when the blocker set is
// recoverable and intact:
//
//   - any other rebase-curable cause present (conflict, failing CI): a base
//     advance genuinely re-decides those, so it must keep unparking. #4058
//     records what happens when it does not — two PRs escalated by a repo-wide
//     CI break stayed parked through five base advances with no reachable exit.
//   - no recoverable blocker set: unpark, exactly as before. Inventing a hold
//     from an absent record would rebuild the permanent park #4038 and #4051
//     each fixed, where pr-remediation excludes escalated PRs upstream so the
//     head can never move and the advertised exit is unreachable.
//   - any recorded blocker resolved (merged, closed, or demoted): unpark. The
//     ordering situation changed, which is the thing worth a cycle.
//   - every recorded blocker still open: hold. The base carries no information
//     about this park.
//
// The head-change and label-removal exits are untouched and remain the
// operator's escape hatches.
func SiblingOrderingUnchanged(
	ctx context.Context,
	provider Provider,
	repo providers.RepositoryRef,
	state RemediationState,
	comments []providers.Comment,
) (bool, error) {
	if !EscalationCausesAreOrderingOnly(state) {
		return false, nil
	}
	blockers := RecordedEscalationBlockers(comments)
	if len(blockers) == 0 {
		return false, nil
	}
	for _, blocker := range blockers {
		blocks, err := NamedBlockerStillBlocks(ctx, provider, repo, blocker)
		if err != nil {
			return false, err
		}
		if !blocks {
			return false, nil
		}
	}
	return true, nil
}

// EscalationCausesAreOrderingOnly reports whether sibling overlap is the only
// rebase-curable cause this park was recorded against. A record with no causes
// at all does not qualify: its cause is unknowable, which is the case
// EscalationBaseAdvanceUnparks deliberately keeps unparking.
func EscalationCausesAreOrderingOnly(state RemediationState) bool {
	ordering := false
	for _, cause := range state.EscalationCauses {
		if cause == RemediationCauseSiblingOverlap {
			ordering = true
			continue
		}
		if BaseAdvanceCuresRemediationCause(cause) {
			return false
		}
	}
	return ordering
}

// RecordedEscalationBlockers recovers the sibling PR numbers an ordering park
// was recorded against, from either marker that can carry them: the
// blocked-on-sibling payload apply-verdict writes, or the BlockingPRs on the
// findings of the latest merge-review verdict.
func RecordedEscalationBlockers(comments []providers.Comment) []int {
	if state, _, found := LatestBlockedOnSiblingState(comments); found && len(state.Blockers) > 0 {
		return state.Blockers
	}
	if verdict, ok := LatestMergeReviewVerdict(comments); ok {
		return UnionBlockingPRs(verdict.Findings)
	}
	return nil
}

// LatestMergeReviewEscalationPins recovers the head/base snapshot merge-review
// recorded when IT applied goobers:merge-escalated. That path (applyverdict.go)
// posts a merge-review-status comment carrying a verdict-json payload and never
// writes a remediation-state comment, so escalationStillBlocks has no
// pr-remediation record to read; the verdict's own SHA pins are the snapshot.
//
// Scans oldest-first (ListComments' order) and keeps the last qualifying
// comment, matching latestRemediationState: only the most recent verdict is
// still actionable. Only a fail verdict qualifies — that is the sole decision
// that escalates — so a later needs-changes or pass comment does not resurrect
// a stale pin.
func LatestMergeReviewEscalationPins(comments []providers.Comment) (head, base string, ok bool) {
	for _, comment := range comments {
		if !IsMergeReviewStatusComment(comment.Body) {
			continue
		}
		verdict, parsed := ParseVerdictComment(comment.Body)
		if !parsed || verdict.Decision != apiv1.VerdictFail || verdict.HeadSHA == "" {
			continue
		}
		head, base, ok = verdict.HeadSHA, verdict.BaseSHA, true
	}
	return head, base, ok
}

// EscalationIsOrderingOnly reports whether a cluster-sibling escalation stands
// on the cluster's merge ORDERING rather than on anything wrong with the PR
// itself — which decides whether a base advance cures the park (#4051).
//
// A pass faults nothing by definition, so an escalation applied over it can
// only be the election deferring this PR behind a sibling. That is the shape
// #4038 missed: it derived the cause with the sequencing predicate (today
// sequencingOnly), which returns false for an empty finding list by design
// (applyverdict.go), and a pass
// carries no findings — so the pass case recorded no cause, never unparked on
// a base advance, and pr-remediation could not move the head to release it
// either because escalated PRs are filtered out upstream. Live on 2026-08-31
// that still refused #3941, #3894, #3900, #3891 and #3968 after #4038 shipped.
//
// For a REJECTING verdict the finding list is what carries the attribution, so
// ask whether any finding blames this PR's own diff. An all-cross-PR-blocked
// verdict does not (that class's RequiresCodeChange is false), which keeps
// #4038's original case working. An EMPTY finding list on a rejecting verdict
// carries no information at all, so it must not be read as "nothing is wrong"
// — it stays parked, preserving #4031's fail-closed contract for a verdict
// that never escalated in the first place.
//
// The severity floor is the lowest one on purpose: any finding that blames
// this PR's own diff keeps the park, because un-parking a PR a reviewer really
// did fault is the worse error.
func EscalationIsOrderingOnly(verdict apiv1.Verdict, prNumber int) bool {
	if verdict.Decision == apiv1.VerdictPass {
		return true
	}
	if len(verdict.Findings) == 0 {
		return false
	}
	return !VerdictHasSubstantiveFindingForPR(&verdict, prNumber, apiv1.SeverityInfo)
}

// LatestMergeReviewVerdict returns the last merge-review verdict on the thread
// that carries a head pin, whatever its decision.
//
// This is the weaker sibling of latestMergeReviewEscalationPins and is only
// consulted when that one finds nothing: a PR escalated by the CLUSTER
// election has no fail verdict of its own, because the fail is recorded on the
// winner's thread while the deferring siblings only get the label (#4038).
// Their own thread still carries a verdict-json payload — needs-changes, or
// even pass — pinned to the head and base the review was computed against, and
// that is a perfectly good snapshot for "has anything moved since?".
//
// Scans oldest-first (ListComments' order) and keeps the last qualifying
// comment, matching latestRemediationState and latestMergeReviewEscalationPins:
// only the most recent verdict is still actionable.
func LatestMergeReviewVerdict(comments []providers.Comment) (apiv1.Verdict, bool) {
	var latest apiv1.Verdict
	found := false
	for _, comment := range comments {
		if !IsMergeReviewStatusComment(comment.Body) {
			continue
		}
		verdict, parsed := ParseVerdictComment(comment.Body)
		if !parsed || verdict.HeadSHA == "" {
			continue
		}
		latest, found = verdict, true
	}
	return latest, found
}

// EscalationBaseAdvanceUnparks reports whether a base-branch advance ALONE may
// release state's park, i.e. whether a rebase onto the new base tip can
// plausibly cure the recorded escalation cause. A head change always releases
// a park regardless of this answer, so every escape hatch (push a commit,
// remove the label) stays reachable for every cause class.
//
// Rebase-curable: a rebase conflict, a sibling-overlap rejection whose
// resolution IS the sibling landing on the base branch, and failing CI. Not
// rebase-curable: a substantive reviewer rejection and a human comment — those
// are properties of the PR's own content, so re-running remediation against an
// unchanged head reproduces the same escalation no matter how far the base has
// moved. Same for infrastructure-failure and policy-excluded outcomes (the PR
// was never evaluated on its merits, and no base advance changes that) and for
// a structural collision, which is escalated precisely because retrying the
// patch cannot resolve it.
func EscalationBaseAdvanceUnparks(state RemediationState) bool {
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
	case RemediationOutcomeInfrastructure, RemediationOutcomePolicyExcluded:
		return false
	}
	if state.StructuralCollisionContext != "" {
		return false
	}
	causes := state.EscalationCauses
	if len(causes) == 0 && !state.EscalationCausesCleared && state.EscalationGeneration == 1 {
		// A park written before #4074 taught the forced path to attribute the
		// reviewer's rejection recorded no escalationCauses, but it recorded
		// attemptedCauses in the same payload — and an escalation is only
		// reachable from a cycle that attempted something. Reading that here is
		// what lets the #4074 exit reach the ALREADY-parked records that
		// motivated it, instead of only PRs parked after the upgrade: a parked
		// record cannot be rewritten, because the rewrite is the checkpoint the
		// park suppresses (#4089).
		//
		// It is the fail-closed direction — AttemptedCauses is cumulative
		// across cycles, a superset of the escalating cycle's causes, so one
		// non-curable member holds the whole park.
		//
		// Bounded to generation 1, the record as first written. A later
		// generation is either a repeat-fail refresh, whose empty cause set is
		// deliberate, or a re-escalation by a binary that populates
		// EscalationCauses itself and so never needs the fallback.
		causes = state.AttemptedCauses
	}
	if len(causes) == 0 {
		// No remediation cause was ever observed — a repeated implementer
		// no-op, or a policy exclusion. Nothing to decide a rebase against, so
		// hold.
		return false
	}
	for _, cause := range causes {
		if !BaseAdvanceCuresRemediationCause(cause) {
			return false
		}
	}
	return true
}

// BaseAdvanceCuresRemediationCause reports whether a base-branch advance since the recorded escalation removes the cause the PR was parked for.
func BaseAdvanceCuresRemediationCause(cause RemediationCause) bool {
	switch cause {
	case RemediationCauseConflict, RemediationCauseSiblingOverlap:
		return true
	case RemediationCauseFailingCI:
		// #4058: CI is evaluated against merge(base, head), not against head
		// alone, so a base advance genuinely re-decides it. Treating a
		// failing-ci park as a property of the PR's own content made every
		// escalation caused BY THE BASE permanent: pr-remediation excludes
		// escalated PRs upstream, so the head can never move, so the "parked
		// until this PR's head changes" exit the escalation comment advertises
		// is unreachable.
		//
		// Live on 2026-08-31 that killed two of the four PRs in the lane.
		// #4040 and #4044 were both escalated on failing-ci by #4045, a
		// repo-wide CI break in which the module-priming step selected the
		// prerelease tag v0.4.0-beta.1 while TestFeatureRegistryAgainstLatestRelease
		// selected v0.3.3 — identical failures on every suite that runs that
		// test, on every PR in the repo, and nothing whatsoever to do with
		// either PR's diff. #4045 was fixed on main and the base advanced five
		// times, and both PRs stayed parked with no reachable exit.
		//
		// Bounded exactly like conflict and sibling-overlap: the retry happens
		// once per base advance, and if CI still fails at the new base the next
		// cycle re-escalates against the new snapshot.
		return true
	default:
		return false
	}
}

// LatestRemediationState scans comments (oldest first, ListComments' own
// order) for the LAST one carrying an embedded remediation-state payload —
// only the most recently recorded cycle/escalation is still actionable —
// and also returns that comment's ID, so a caller can edit it in place
// (postOrUpdateStickyComment) rather than posting a new one. found is false
// if no comment in the thread carries a payload (the PR's first
// pr-remediation cycle), not an error.
func LatestRemediationState(comments []providers.Comment) (state RemediationState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := ParseRemediationStateComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return RemediationState{}, "", false
}

// LatestRemediationStateForPR is LatestRemediationState over the comments of one PR.
func LatestRemediationStateForPR(body string, comments []providers.Comment) (state RemediationState, commentID string, found bool) {
	if s, ok := ParseRemediationStateComment(body); ok {
		state, found = s, true
	}
	if s, id, ok := LatestRemediationState(comments); ok {
		return s, id, true
	}
	return state, "", found
}
