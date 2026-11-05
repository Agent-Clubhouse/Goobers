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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/remediationselect"
	"github.com/goobers/goobers/providers"
)

const siblingOverlapLookback = 30 * 24 * time.Hour

// remediationSupersededAllowance is how many consecutive superseded cycles
// (#6019) a PR may refund before the checkpoint parks it for a human.
const remediationSupersededAllowance = 3

// remediationInfrastructureAllowance is how many consecutive
// infrastructure-voided cycles a PR may refund before the checkpoint parks it
// with escalationOutcome infrastructure-failure (#5588).
const remediationInfrastructureAllowance = 3

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
		exit := fmt.Sprintf("Parked until %s, or a human removes `%s`.", unpark, remediationEscalatedLabel)
		if state.NoopGuardRepark {
			// The unchanged-digest guard requires TWO label clears to release
			// this park: the first is a cheap no-op (#716/#2378) so a stray
			// bot re-label doesn't undo an operator's intent, and only the
			// second actually triggers a fresh remediation attempt.
			exit = fmt.Sprintf(
				"Parked until %s, or a human removes `%s` (this guard needs the label removed twice — the first removal is a no-op check, the second releases the park).",
				unpark, remediationEscalatedLabel,
			)
		}
		prose = fmt.Sprintf("**pr-remediation escalated**\n\n%s. %s", state.EscalatedReason, exit)
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
		if state.InfrastructureVoided {
			prose += " This cycle's run ended on an infrastructure failure before its fix was evaluated, so its attempt will be refunded."
		}
		if state.Superseded {
			prose += " This cycle's run was superseded: the pull request's head moved under it before its fix was published, so its attempt will be refunded."
		}
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

type remediationCheckpointStructuralCollisionFinder func(context.Context, providers.PullRequestSummary, string, string, []rebaseConflictLocation, string, string) ([]structuralCollision, error)
type remediationCheckpointNoopGuard func(string, int, *providers.PullRequestSummary, string) (string, error)
type remediationCheckpointSiblingOverlapFinder func(context.Context, int, string, string, []providers.PullRequestSummary) (string, error)
type remediationCheckpointBaseTipReader func(context.Context, string) (string, error)
type remediationCheckpointBaseCheckStateReader func(context.Context, string) (providers.CheckState, error)

// remediationCheckpointFeatures declares enrichments available to a provider.
// ADO leaves GitHub-only enrichments nil rather than branching the stage body.
type remediationCheckpointFeatures struct {
	CopyListedLabels         bool
	StabilizeLiveReads       bool
	CheckoutAuth             gitAuthEnvironmentResolver
	CheckoutToken            string
	CommentNoun              string
	FindStructuralCollisions remediationCheckpointStructuralCollisionFinder
	NoopEscalationReason     remediationCheckpointNoopGuard
	SiblingOverlapContext    remediationCheckpointSiblingOverlapFinder
	LiveBaseTip              remediationCheckpointBaseTipReader
	// BaseCheckState answers "is required CI red on the base branch itself?",
	// which is what separates a failing-CI cause the PR's own diff caused from
	// one it did not (#2701). Left nil by a provider with no check-state API,
	// which simply keeps the pre-#2701 behavior of remediating every failing-CI
	// cause agentically.
	BaseCheckState remediationCheckpointBaseCheckStateReader
}

func githubRemediationCheckpointFeatures(provider remediationProvider, repo providers.RepositoryRef, pushToken string) remediationCheckpointFeatures {
	return remediationCheckpointFeatures{
		StabilizeLiveReads: true,
		CheckoutAuth:       tokenGitAuthEnvironment(pushToken),
		CheckoutToken:      pushToken,
		CommentNoun:        "comments",
		FindStructuralCollisions: func(ctx context.Context, current providers.PullRequestSummary, base, headPrefix string, locations []rebaseConflictLocation, pushToken, rebaseBaseSHA string) ([]structuralCollision, error) {
			return findStructuralCollisions(ctx, provider, repo, current, base, headPrefix, locations, ".", pushToken, rebaseBaseSHA)
		},
		NoopEscalationReason: remediationNoopEscalationReason,
		SiblingOverlapContext: func(ctx context.Context, selectedNumber int, base, headPrefix string, openPRs []providers.PullRequestSummary) (string, error) {
			overlaps, err := knownSiblingOverlapFindings(ctx, provider, repo, selectedNumber, base, headPrefix, openPRs, time.Now().UTC().Add(-siblingOverlapLookback))
			if err != nil {
				return "", err
			}
			return renderSiblingOverlapContext(overlaps), nil
		},
		LiveBaseTip: func(ctx context.Context, base string) (string, error) {
			return provider.BranchTipSHA(ctx, repo, base)
		},
		BaseCheckState: func(ctx context.Context, base string) (providers.CheckState, error) {
			tip, err := provider.BranchTipSHA(ctx, repo, base)
			if err != nil {
				return "", err
			}
			return provider.RefCheckState(ctx, repo, tip)
		},
	}
}

func adoRemediationCheckpointFeatures() (remediationCheckpointFeatures, error) {
	gitAuth, err := adoRemediationGitAuthEnvironment()
	if err != nil {
		return remediationCheckpointFeatures{}, err
	}
	return remediationCheckpointFeatures{
		CopyListedLabels: true,
		CheckoutAuth:     gitAuth,
		CommentNoun:      "thread comments",
	}, nil
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
	Prior          remediationState
	Causes         []remediationCause
	Budgets        remediationBudgets
	Digest         string
	HeadSHA        string
	BaseSHA        string
	Watermark      string
	Forced         bool
	ForcedReason   string
	ForcedOutcome  remediationEscalationOutcome
	PolicyExcluded bool
	// ExternallyBlocked marks a cycle whose every detected cause is external to
	// the PR's own diff (#2701), with ExternalBlockReason naming which ones.
	// It parks the PR here instead of spending the agentic chain on a change no
	// in-worktree edit can make.
	ExternallyBlocked          bool
	ExternalBlockReason        string
	StructuralCollisions       []structuralCollision
	StructuralCollisionContext string
	// RunID is the run recording this cycle, stamped on an advancing state so
	// its charge can be voided if the run dies on infrastructure (#5588).
	RunID string
}

type remediationCheckpointDecision struct {
	State            remediationState
	Escalation       remediationEscalation
	Escalated        bool
	HasObservedCause bool
}

func decideRemediationCheckpoint(in remediationCheckpointDecisionInput) remediationCheckpointDecision {
	retry, retryInfraFailures := rewindOwnCheckpointWrite(&in)
	supersededCycles := countSupersededCycle(&in)
	infraFailures := settleInfrastructureVoidedCycle(&in)
	if retry {
		infraFailures = retryInfraFailures
	}
	stalled := remediationAttempted(in.Prior.AttemptsByCause) && remediationStalled(in.Prior, in.Digest, in.BaseSHA)
	exhaustedCause, exceeded := exhaustedRemediationCause(in.Prior.AttemptsByCause, in.Causes, in.Budgets)
	structuralCollision := len(in.StructuralCollisions) > 0
	// A concrete in-run finding always outranks the external classification:
	// an exhausted budget, a stalled diff or a structural collision is the more
	// specific account of why this PR is parked, and a forced escalation
	// carries a caller-supplied terminal reason.
	externallyBlocked := in.ExternallyBlocked && !exceeded && !stalled && !structuralCollision && !in.Forced
	cycles := in.Prior.Cycles + 1

	if exceeded || stalled || structuralCollision || in.Forced || externallyBlocked {
		attempted := in.Prior.AttemptsByCause
		if structuralCollision {
			for _, cause := range in.Causes {
				attempted.Increment(cause)
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
				in.Prior.AttemptsByCause.ForCause(exhaustedCause),
				in.Budgets.ForCause(exhaustedCause),
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
		if externallyBlocked {
			// No agent ran this cycle, exactly like a policy exclusion — the
			// park is a statement about state outside this PR, so it consumes
			// no cause allowance. EscalationCauses stays the observed external
			// cause set below, which is what lets escalationBaseAdvanceUnparks
			// release the park automatically once the base advances (the
			// sibling landed, or CI on the base recovered).
			escalation.Outcome = remediationOutcomeExternallyBlocked
			escalation.Attempted = false
			escalation.Reason = in.ExternalBlockReason
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
			// #4074: a forced escalation means the IN-RUN REVIEWER rejected the
			// remediation ATTEMPT, not the PR. Discarding the cause made
			// escalationBaseAdvanceUnparks read the record as "no remediation
			// cause was ever observed" and refuse every base advance forever —
			// and since filterRemediationPullRequests excludes escalated PRs,
			// the head could never move either, so the exit the escalation
			// comment advertises was unreachable.
			//
			// What the reviewer actually rejected is whatever remediation was
			// attempting, and this same decision already computed that two
			// lines up. A rejected conflict resolution is a function of the
			// base: the next base advance produces a different one. A rejected
			// substantive fix is not.
			//
			// AttemptedCauses is the CUMULATIVE set across cycles, which is the
			// fail-closed direction — any single non-curable cause in it keeps
			// the whole park. Live on 2026-08-31 this told #3894 and #3891
			// (attempted `conflict` only, both displaced by a sibling merge)
			// apart from #3941 (attempted `substantive`), which stays parked.
			escalationCauses = escalation.AttemptedCauses
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
		attempts.Increment(cause)
	}
	return remediationCheckpointDecision{
		HasObservedCause: len(in.Causes) > 0,
		State: remediationState{
			Cycles: cycles, AttemptsByCause: attempts, LastDiffDigest: in.Digest,
			HeadSHA: in.HeadSHA, BaseSHA: in.BaseSHA, LastSeenCommentAt: in.Watermark,
			RunID: in.RunID, ChargedCauses: slices.Clone(in.Causes),
			InfrastructureFailures: infraFailures,
			SupersededCycles:       supersededCycles,
		},
	}
}

// countSupersededCycle counts a superseded prior cycle (#6019) toward the
// consecutive bound and returns the count to carry forward — 0 once a cycle
// is not superseded. At remediationSupersededAllowance it forces a
// did-not-converge park: each of those cycles may have spent a full agent
// session before the move was noticed, and the refund (settled by
// settleInfrastructureVoidedCycle) hides every one of them from the cause
// budget and the same-diff stall.
func countSupersededCycle(in *remediationCheckpointDecisionInput) int {
	prior := in.Prior
	if prior.Escalated || !prior.Superseded {
		return 0
	}
	count := prior.SupersededCycles + 1
	if count >= remediationSupersededAllowance && !in.Forced {
		in.Forced = true
		in.ForcedOutcome = remediationOutcomeDidNotConverge
		in.ForcedReason = fmt.Sprintf(
			"the pull request's head moved under each of the last %d remediation cycles before their fix could be published; remediation cannot keep up with concurrent pushes to it",
			count,
		)
	}
	return count
}

func remediationAttempted(attempts remediationAttempts) bool {
	for _, cause := range remediationCauseOrder {
		if attempts.ForCause(cause) > 0 {
			return true
		}
	}
	return false
}

// rewindOwnCheckpointWrite makes a retry of the same checkpoint attempt
// idempotent (#6008). remediation-checkpoint writes its sticky state (the
// charged causes and this cycle's diff digest) before the agent runs, so a
// stage retry after that write reads its OWN record back as the prior cycle:
// the byte-identical digest tripped the same-diff stall and parked a PR that
// was making progress, and the causes were charged a second time (which could
// also trip budget-exhausted). When the prior advancing record was written by
// this same run at the same head and base, rewind it to the state before that
// write: refund what it charged, forget its digest (the first attempt already
// passed the stall check against the cycle before it) and undo its cycle
// count. The decision below then reproduces the first attempt's advancing
// state. A forced (--escalate) call is never a retry: the park stages invoke
// it in the same run, at the same head, right after this attempt's own
// checkpoint, and it must keep the attempt's charge so the escalation
// attributes what was attempted (#4074). Returns whether it rewound, and the
// consecutive-infrastructure count the rewound record carried so the rewrite
// preserves it.
func rewindOwnCheckpointWrite(in *remediationCheckpointDecisionInput) (bool, int) {
	prior := in.Prior
	if in.Forced || in.RunID == "" || prior.RunID != in.RunID || prior.Escalated || prior.InfrastructureVoided || prior.Superseded ||
		prior.HeadSHA != in.HeadSHA || prior.BaseSHA != in.BaseSHA {
		return false, 0
	}
	for _, cause := range prior.ChargedCauses {
		prior.AttemptsByCause.Refund(cause)
	}
	prior.LastDiffDigest = ""
	if prior.Cycles > 0 {
		prior.Cycles--
	}
	in.Prior = prior
	return true, prior.InfrastructureFailures
}

// settleInfrastructureVoidedCycle settles the prior cycle's provisional
// charge (#5588/#5598). A prior cycle whose run the failed-terminal handler
// marked InfrastructureVoided never had its fix evaluated, so its
// ChargedCauses are refunded and its pre-agent digest is dropped from the
// same-diff comparison (the diff did not move because nothing ran, not because
// the agent could not move it). Consecutive voided cycles are counted, and the
// remediationInfrastructureAllowance-th one forces an infrastructure-failure
// park so a PR whose runs always die before evaluation cannot loop forever.
// Returns the consecutive voided-cycle count to carry forward (0 once a cycle
// settles normally). A prior that was not voided is left untouched.
//
// A prior cycle marked Superseded (#6019) — its run ended because the PR
// moved under it — is refunded the same way, but carries the consecutive
// infrastructure count forward unchanged: it is neither another
// infrastructure failure nor evidence the substrate recovered.
func settleInfrastructureVoidedCycle(in *remediationCheckpointDecisionInput) int {
	prior := in.Prior
	if prior.Escalated || (!prior.InfrastructureVoided && !prior.Superseded) {
		return 0
	}
	for _, cause := range prior.ChargedCauses {
		prior.AttemptsByCause.Refund(cause)
	}
	prior.LastDiffDigest = ""
	in.Prior = prior
	if !prior.InfrastructureVoided {
		return prior.InfrastructureFailures
	}
	infraFailures := prior.InfrastructureFailures + 1
	if infraFailures >= remediationInfrastructureAllowance && !in.Forced {
		in.Forced = true
		in.ForcedOutcome = remediationOutcomeInfrastructure
		in.ForcedReason = fmt.Sprintf(
			"the last %d remediation cycles each ended on an infrastructure failure before their fix could be evaluated; no implementation defect was established",
			infraFailures,
		)
	}
	return infraFailures
}

func remediationCheckpointMoot(stdout, stderr io.Writer, selectedNumber int) int {
	pln(stdout, "PR is no longer open (merged/closed since selection) — checkpoint moot, nothing to record")
	if err := writeCheckpointResult(stderr, false, selectedNumber, "", "", remediationEscalation{}); err != nil {
		return 1
	}
	return 0
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
	state.EscalationCausesCleared = true
	return provider.UpdateComment(ctx, repo, commentID, renderRemediationComment(state))
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
	"the active cause exhausts its DSL-declared budget, on a byte-identical\n" +
	"repeat, or when every detected cause is external to the PR's own diff\n" +
	"(sibling sequencing, or CI already red on the base branch), or record the advanced\n" +
	"state as a new sticky comment. Requires selectedNumber (inputsFrom\n" +
	"gather-pr-context's selectedNumber output) and remediationCauses. Each of\n" +
	"the five per-cause budget inputs defaults to 2 when undeclared\n" +
	"(goobers validate warns). --budget overrides every declared cause\n" +
	"for standalone diagnostics. --escalation-outcome classifies a forced\n" +
	"--escalate as did-not-converge (the default), budget-exhausted, or infrastructure-failure.\n" +
	"Escalations persist a machine-readable `escalationOutcome`\n" +
	"(`did-not-converge`, `budget-exhausted`, `policy-excluded`, `externally-blocked`, or\n" +
	"`infrastructure-failure`), whether\n" +
	"repair was attempted, and the attempted causes in both the sticky comment\n" +
	"and stage result. Exit codes: 0 = checkpoint\n" +
	"recorded (escalated or not — both are normal outcomes), 1 = business\n" +
	"error, 2 = usage/IO error.\n"

// remediationCheckpointFlags is the parsed, validated CLI surface of
// remediation-checkpoint.
type remediationCheckpointFlags struct {
	pathArg        string
	budgetOverride int
	escalateReason string
	forcedOutcome  remediationEscalationOutcome
}

func parseRemediationCheckpointFlags(args []string, stderr io.Writer) (remediationCheckpointFlags, int, bool) {
	fs := newCLIFlagSet("remediation-checkpoint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "remediation-checkpoint")
	budgetOverride := fs.Int("budget", 0, "override every DSL-declared per-cause budget (standalone diagnostics)")
	escalateReason := fs.String("escalate", "", "escalate unconditionally with this reason, skipping the D4/D5 checks")
	escalationOutcome := fs.String(
		"escalation-outcome",
		string(remediationOutcomeDidNotConverge),
		"machine-readable outcome for --escalate (did-not-converge, budget-exhausted, or infrastructure-failure)",
	)
	if err := fs.Parse(args); err != nil {
		return remediationCheckpointFlags{}, 2, false
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return remediationCheckpointFlags{}, 2, false
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
		return remediationCheckpointFlags{}, 2, false
	}
	if *escalateReason == "" && forcedOutcome != remediationOutcomeDidNotConverge {
		pf(stderr, "error: --escalation-outcome requires --escalate\n")
		return remediationCheckpointFlags{}, 2, false
	}
	return remediationCheckpointFlags{
		pathArg:        pathArg,
		budgetOverride: *budgetOverride,
		escalateReason: *escalateReason,
		forcedOutcome:  forcedOutcome,
	}, 0, true
}

func resolveCheckpointSelectedNumber(root string, stderr io.Writer) (int, int, bool) {
	if selectedNumberStr := providerInput("selectedNumber", ""); selectedNumberStr != "" {
		n, err := strconv.Atoi(selectedNumberStr)
		if err != nil {
			pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
			return 0, 1, false
		}
		return n, 0, true
	}
	// Ledger fallback (#392): in --escalate mode this stage runs after the
	// agentic chain, where Task.InputsFrom can no longer reach
	// gather-pr-context's selectedNumber — implement and local-ci each
	// became the upstream in turn. The run's own PR claim is the durable
	// answer. Still an error if there is no claim either: escalating
	// without knowing WHICH PR would label an arbitrary one.
	n, ok, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 0, 1, false
	}
	if !ok {
		pf(stderr, "error: selectedNumber is required (inputsFrom gather-pr-context's selectedNumber output, or a PR claim held by this run)\n")
		return 0, 1, false
	}
	return n, 0, true
}

// remediationCheckpointRunEnv carries the resolved shared checkpoint state.
// Provider-specific concerns remain behind the reader, transport, and feature
// adapters supplied by runRemediationCheckpoint.
type remediationCheckpointRunEnv struct {
	ctx                context.Context
	root               string
	repo               providers.RepositoryRef
	reader             remediationCheckpointReader
	transport          remediationCheckpointTransport
	features           remediationCheckpointFeatures
	pushToken          string
	gitAuth            gitAuthEnvironmentResolver
	selectedNumber     int
	base               string
	headPrefix         string
	evaluatedBaseSHA   string
	evaluatedBaseFixed bool
	prs                []providers.PullRequestSummary
	current            *providers.PullRequestSummary
	stdout             io.Writer
	stderr             io.Writer
}

func openRemediationCheckpointEnv(
	ctx context.Context,
	root string,
	repo providers.RepositoryRef,
	selectedNumber int,
	reader remediationCheckpointReader,
	transport remediationCheckpointTransport,
	features remediationCheckpointFeatures,
	stdout, stderr io.Writer,
) (*remediationCheckpointRunEnv, int, bool) {
	env := &remediationCheckpointRunEnv{
		ctx:            ctx,
		root:           root,
		repo:           repo,
		reader:         reader,
		transport:      transport,
		features:       features,
		selectedNumber: selectedNumber,
		base:           providerInput("base", providerBaseBranch()),
		headPrefix:     providerInput("headPrefix", providerBranchNamespace()),
		stdout:         stdout,
		stderr:         stderr,
	}
	prs, err := reader.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: env.base, HeadPrefix: env.headPrefix, SkipCheckState: true,
	})
	if err != nil {
		return nil, failProviderStage(stderr, "list pull requests", err, ""), false
	}
	env.prs = prs
	var listed *providers.PullRequestSummary
	for i := range prs {
		if prs[i].Number == selectedNumber {
			listed = &prs[i]
			break
		}
	}
	if listed == nil {
		return nil, remediationCheckpointMoot(stdout, stderr, selectedNumber), false
	}
	if code, ok := env.refreshCurrent(fmt.Sprintf("get pull request #%d", selectedNumber)); !ok {
		return nil, code, false
	}
	if features.CopyListedLabels {
		env.current.Labels = listed.Labels
	}
	return env, 0, true
}

func (env *remediationCheckpointRunEnv) livePullRequest(what string) (providers.PullRequestSummary, int, bool) {
	live, err := env.reader.GetPullRequest(env.ctx, env.repo, strconv.Itoa(env.selectedNumber))
	if err != nil {
		return providers.PullRequestSummary{}, failProviderStage(env.stderr, what, err, ""), false
	}
	if live.State != "open" || live.Merged {
		return providers.PullRequestSummary{}, remediationCheckpointMoot(env.stdout, env.stderr, env.selectedNumber), false
	}
	return live, 0, true
}

func (env *remediationCheckpointRunEnv) refreshCurrent(what string) (int, bool) {
	live, code, ok := env.livePullRequest(what)
	if !ok {
		return code, false
	}
	env.current = &live
	return 0, true
}

// remediationCheckpointMode is the loop-control mode this cycle runs in. A
// forced escalation consults no digest and touches no git, so every phase
// downstream branches on it.
type remediationCheckpointMode struct {
	forced           bool
	reason           string
	policyExcluded   bool
	conflicted       bool
	attemptedHeadSHA string
	forcedOutcome    remediationEscalationOutcome
	causes           []remediationCause
	budgets          remediationBudgets
	// rebaseInfrastructureFailure is set when rebase-pr's own attempt failed
	// on an infra fault (a git subprocess we ran, a network blip) rather than
	// reaching a real conflict/substantive verdict about the PR (#4173).
	// rebase-pr already withholds remediationCauses on that path, so mode.causes
	// stays empty here too — this field is what lets runRemediationCheckpointCore
	// tell that empty-causes apart from an ordinary clean cycle and skip the
	// digest-stall/escalation machinery instead of misreading a rebase retry's
	// unchanged diff as a stalled remediation attempt.
	rebaseInfrastructureFailure bool
}

func resolveRemediationCheckpointMode(flags remediationCheckpointFlags, features remediationCheckpointFeatures, stderr io.Writer) (remediationCheckpointMode, int, bool) {
	conflicted := false
	attemptedHeadSHA := ""
	if features.FindStructuralCollisions != nil {
		var err error
		conflicted, err = strconv.ParseBool(providerInput("conflict", "false"))
		if err != nil {
			pf(stderr, "error: invalid conflict input: %v\n", err)
			return remediationCheckpointMode{}, 1, false
		}
		attemptedHeadSHA = providerInput("attemptedHeadSha", "")
	}
	// A rebase-pr cycle whose only detected cause(s) the declared `remediate`
	// policy excludes (#941/PRR-6) escalates immediately here, exactly like
	// an explicit --escalate: no agent ever ran on this cycle, so budget/
	// no-progress checks would just delay the same inevitable outcome.
	// --escalate (the reviewer-fail path, §4 D2) wins if somehow both are
	// set — it is the more specific, caller-supplied terminal reason.
	policyExcluded, err := strconv.ParseBool(providerInput("policyExcluded", "false"))
	if err != nil {
		pf(stderr, "error: invalid policyExcluded input: %v\n", err)
		return remediationCheckpointMode{}, 1, false
	}
	rebaseInfrastructureFailure, err := strconv.ParseBool(providerInput("rebaseInfrastructureFailure", "false"))
	if err != nil {
		pf(stderr, "error: invalid rebaseInfrastructureFailure input: %v\n", err)
		return remediationCheckpointMode{}, 1, false
	}
	mode := remediationCheckpointMode{
		reason:                      flags.escalateReason,
		policyExcluded:              policyExcluded,
		conflicted:                  conflicted,
		attemptedHeadSHA:            attemptedHeadSHA,
		forcedOutcome:               flags.forcedOutcome,
		rebaseInfrastructureFailure: rebaseInfrastructureFailure,
	}
	if mode.reason == "" && policyExcluded {
		mode.reason = providerInput("policyExcludedReason", "declared remediation policy excludes the only detected cause(s)")
	}
	mode.forced = mode.reason != ""
	if flags.budgetOverride < 0 {
		pf(stderr, "error: --budget must not be negative\n")
		return remediationCheckpointMode{}, 1, false
	}
	if mode.forced {
		return mode, 0, true
	}
	rawCauses := providerInput("remediationCauses", "")
	if strings.TrimSpace(rawCauses) == "" {
		return mode, 0, true
	}
	causes, err := parseRemediationCauses(rawCauses)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return remediationCheckpointMode{}, 1, false
	}
	if providerInput("ciStatus", "") == string(providers.CheckStatePassing) {
		causes = slices.DeleteFunc(causes, func(cause remediationCause) bool {
			return cause == remediationCauseFailingCI
		})
	}
	budgets, err := declaredRemediationBudgets(flags.budgetOverride)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return remediationCheckpointMode{}, 1, false
	}
	mode.causes = causes
	mode.budgets = budgets
	return mode, 0, true
}

func (env *remediationCheckpointRunEnv) recordSequencingOnlyWait() int {
	if hasAnyLabel(env.current.Labels, []string{needsRemediationLabel}) {
		if err := env.transport.UpdateLabels(env.ctx, nil, []string{needsRemediationLabel}); err != nil {
			return failProviderStage(env.stderr, fmt.Sprintf("clear needs-remediation label from sequencing-only PR #%d", env.selectedNumber), err, "")
		}
	}
	if err := writeCheckpointResult(env.stderr, false, env.selectedNumber, env.current.Head, env.current.HeadSHA, remediationEscalation{}); err != nil {
		return 1
	}
	pf(env.stdout, "PR #%d is blocked only on sibling sequencing — waiting without consuming remediation budget\n", env.selectedNumber)
	return 0
}

// recordInfrastructureNoop records a cycle in which rebase-pr's own attempt
// failed on an infra fault rather than reaching any verdict about the PR
// (#4173). Unlike recordSequencingOnlyWait, it deliberately leaves
// needsRemediationLabel in place: the fault is on our side, and clearing the
// label would keep this PR from being reselected for the retry that a
// transient infra fault is expected to clear on its own.
func (env *remediationCheckpointRunEnv) recordInfrastructureNoop() int {
	if err := writeCheckpointResult(env.stderr, false, env.selectedNumber, env.current.Head, env.current.HeadSHA, remediationEscalation{}); err != nil {
		return 1
	}
	pf(env.stdout, "PR #%d: rebase-pr failed on an infrastructure fault — waiting for a retry without consuming remediation budget or attributing a cause\n", env.selectedNumber)
	return 0
}

func (env *remediationCheckpointRunEnv) resolveEvaluatedBaseSHA(mode remediationCheckpointMode) (int, bool) {
	env.evaluatedBaseSHA = env.current.BaseSHA
	env.evaluatedBaseFixed = false
	if mode.forced {
		return 0, true
	}
	if rebaseBaseSHA := strings.TrimSpace(providerInput("rebaseBaseSha", "")); rebaseBaseSHA != "" {
		env.evaluatedBaseSHA = rebaseBaseSHA
		env.evaluatedBaseFixed = true
		return 0, true
	}
	if env.features.LiveBaseTip == nil {
		return 0, true
	}
	liveBaseSHA, err := env.features.LiveBaseTip(env.ctx, env.base)
	if err != nil {
		return failProviderStage(
			env.stderr,
			fmt.Sprintf("resolve evaluated base branch %q tip for PR #%d", env.base, env.selectedNumber),
			err,
			"",
		), false
	}
	if strings.TrimSpace(liveBaseSHA) == "" {
		return failProviderStage(
			env.stderr,
			fmt.Sprintf("resolve evaluated base branch %q tip for PR #%d", env.base, env.selectedNumber),
			fmt.Errorf("provider returned an empty base SHA"),
			"",
		), false
	}
	env.evaluatedBaseSHA = liveBaseSHA
	return 0, true
}

// remediationCheckpointObservation is what one stabilized checkpoint read saw:
// the diff digest this cycle compares against the recorded one, any structural
// collision behind a conflict, and the comment thread the sticky state is read
// back from.
type remediationCheckpointObservation struct {
	digest               string
	structuralCollisions []structuralCollision
	comments             []providers.Comment
}

// checkoutAndDigest re-checkouts the PR's own branch when needed and hashes its
// diff against the base.
//
// This stage gets its OWN fresh worktree (internal/runner's buildEnvelope keys
// worktree continuity on the run's shared branch, not on whatever an earlier
// stage locally checked out — #133/#363's own re-checkout for the same reason),
// so gather-pr-context's or rebase-pr's checkout does not survive to here.
// Without this, diffDigest would diff against whatever this fresh worktree
// defaulted to (the run's own untouched base checkout), not the PR's actual
// just-pushed content.
// ...UNLESS this worktree is already on that branch, which since #392 is the
// normal case: gather-pr-context rebinds the run's workspace branch to the PR's
// head, so the runner provisions this stage on it directly.
//
// Re-checking-out anyway would be actively destructive rather than merely
// redundant. checkoutExistingBranch is `git checkout -B <branch> FETCH_HEAD` —
// a hard reset to the REMOTE tip — and on the substantive-findings path
// rebase-pr deliberately does NOT push its rebase (rebasepr.go's `!conflict &&
// !hasSubstantiveFindings` guard), so that rebase exists only as a local commit
// on this very branch. Resetting here would discard it, hand `implement` an
// un-rebased tree, and leave the PR still behind its base after a successful
// remediation — looping every such PR until the D4 budget escalated it.
//
// A forced escalation (--escalate) skips all of it in the caller: the caller
// already has a terminal reason, so no digest is consulted to reach the
// decision. Doing the fetch anyway would let a transient git/network failure
// exit non-zero BEFORE the labelling below — leaving goobers:merge-escalated
// unapplied and goobers:needs-remediation still set, so the PR is re-selected
// and the reviewer re-rejects the same approach, which is exactly what §4 D2's
// escalate-immediately rule exists to prevent.
func (env *remediationCheckpointRunEnv) checkoutAndDigest(
	mode remediationCheckpointMode,
	forceHeadRefresh, forceBaseRefresh, attemptMatchesLiveHead bool,
) (string, bool, int, bool) {
	onBranch, err := currentBranchIs(".", env.current.Head)
	if err != nil {
		pf(env.stderr, "error: resolve current branch for PR #%d: %v\n", env.selectedNumber, err)
		return "", attemptMatchesLiveHead, 1, false
	}
	refreshBranch := forceHeadRefresh || !onBranch
	if onBranch && mode.conflicted && mode.attemptedHeadSHA != "" {
		localHeadSHA, err := resolveHead(".")
		if err != nil {
			pf(env.stderr, "error: resolve current head for PR #%d: %v\n", env.selectedNumber, err)
			return "", attemptMatchesLiveHead, 1, false
		}
		// A conflicted rebase was aborted, so there is no local rebase to
		// preserve when a concurrent push makes this checkout stale.
		refreshBranch = refreshBranch || localHeadSHA != env.current.HeadSHA
	}
	if refreshBranch {
		fetchedSHA, err := checkoutExistingBranchWithAuth(env.ctx, ".", env.current.Head, env.gitAuth)
		if err != nil {
			pf(env.stderr, "error: checkout PR #%d's branch %q: %v\n", env.selectedNumber, env.current.Head, err)
			return "", attemptMatchesLiveHead, 1, false
		}
		env.current.HeadSHA = fetchedSHA
		if mode.conflicted && mode.attemptedHeadSHA != "" {
			attemptMatchesLiveHead = mode.attemptedHeadSHA == fetchedSHA
		}
	}
	if forceBaseRefresh {
		if _, err := fetchExistingBranchWithAuth(env.ctx, ".", env.current.Base, env.gitAuth); err != nil {
			pf(env.stderr, "error: refresh PR #%d's base branch %q: %v\n", env.selectedNumber, env.current.Base, err)
			return "", attemptMatchesLiveHead, 1, false
		}
	}
	digest, err := diffDigest(".", env.evaluatedBaseSHA)
	if err != nil {
		pf(env.stderr, "error: compute diff digest for PR #%d: %v\n", env.selectedNumber, err)
		return "", attemptMatchesLiveHead, 1, false
	}
	return digest, attemptMatchesLiveHead, 0, true
}

func (env *remediationCheckpointRunEnv) collectStructuralCollisions() ([]structuralCollision, int, bool) {
	conflictLocations, err := decodeConflictLocations(providerInput("conflictLocations", ""))
	if err != nil {
		pf(env.stderr, "error: %v\n", err)
		return nil, 1, false
	}
	collisions, err := env.features.FindStructuralCollisions(
		env.ctx, *env.current, env.base, env.headPrefix, conflictLocations,
		env.pushToken, env.evaluatedBaseSHA,
	)
	if err != nil {
		return nil, failProviderStage(env.stderr, fmt.Sprintf("detect same-function structural collision for PR #%d", env.selectedNumber), err, ""), false
	}
	return collisions, 0, true
}

// observeOnce is stabilize's single-read path for providers whose live reads
// do not need re-validation: one digest (unless forced) plus the comment thread.
func (env *remediationCheckpointRunEnv) observeOnce(mode remediationCheckpointMode) (remediationCheckpointObservation, int, bool) {
	var observation remediationCheckpointObservation
	if !mode.forced {
		digest, _, code, ok := env.checkoutAndDigest(mode, false, false, false)
		if !ok {
			return observation, code, false
		}
		observation.digest = digest
	}
	comments, err := env.transport.ListComments(env.ctx)
	if err != nil {
		return observation, failProviderStage(env.stderr, fmt.Sprintf("list %s on PR #%d", env.features.CommentNoun, env.selectedNumber), err, ""), false
	}
	observation.comments = comments
	return observation, 0, true
}

// refreshEvaluatedBaseSHA re-resolves the live-base fallback after a stabilize
// read and reports whether it moved. A rebase-provided base is fixed for the
// cycle and forced checkpoints never compare digests, so neither re-resolves.
func (env *remediationCheckpointRunEnv) refreshEvaluatedBaseSHA(mode remediationCheckpointMode) (bool, int, bool) {
	if mode.forced || env.evaluatedBaseFixed {
		return false, 0, true
	}
	previous := env.evaluatedBaseSHA
	if code, ok := env.resolveEvaluatedBaseSHA(mode); !ok {
		return false, code, false
	}
	return env.evaluatedBaseSHA != previous, 0, true
}

// stabilize reads the checkpoint's inputs (branch digest, structural
// collisions, comment thread) and re-reads them until the PR's head and base
// stop moving underneath the read, so the recorded state pairs one head/base
// with the digest actually computed for it.
func (env *remediationCheckpointRunEnv) stabilize(mode remediationCheckpointMode) (remediationCheckpointObservation, int, bool) {
	if !env.features.StabilizeLiveReads {
		return env.observeOnce(mode)
	}

	const maxCheckpointRefreshes = 3
	var observation remediationCheckpointObservation
	forceHeadRefresh := false
	forceBaseRefresh := env.evaluatedBaseSHA != env.current.BaseSHA
	for refreshAttempt := 0; refreshAttempt < maxCheckpointRefreshes; refreshAttempt++ {
		attemptMatchesLiveHead := false
		if mode.conflicted && !mode.forced && mode.attemptedHeadSHA != "" {
			if code, ok := env.refreshCurrent(fmt.Sprintf("get live state for PR #%d", env.selectedNumber)); !ok {
				return observation, code, false
			}
			attemptMatchesLiveHead = mode.attemptedHeadSHA == env.current.HeadSHA
		}

		observation.digest = ""
		if !mode.forced {
			digest, matched, code, ok := env.checkoutAndDigest(mode, forceHeadRefresh, forceBaseRefresh, attemptMatchesLiveHead)
			if !ok {
				return observation, code, false
			}
			observation.digest = digest
			attemptMatchesLiveHead = matched
		}

		observation.structuralCollisions = nil
		if mode.conflicted && !mode.forced && attemptMatchesLiveHead {
			collisions, code, ok := env.collectStructuralCollisions()
			if !ok {
				return observation, code, false
			}
			observation.structuralCollisions = collisions
		}

		comments, err := env.transport.ListComments(env.ctx)
		if err != nil {
			return observation, failProviderStage(env.stderr, fmt.Sprintf("list %s on PR #%d", env.features.CommentNoun, env.selectedNumber), err, ""), false
		}
		observation.comments = comments
		// Keep this read independent from current: assigning through the value
		// current points at would pair newly read metadata with the old digest.
		lateCurrent, code, ok := env.livePullRequest(fmt.Sprintf("get pull request #%d", env.selectedNumber))
		if !ok {
			return observation, code, false
		}
		headChanged := lateCurrent.Head != env.current.Head || lateCurrent.HeadSHA != env.current.HeadSHA
		baseChanged := lateCurrent.Base != env.current.Base || lateCurrent.BaseSHA != env.current.BaseSHA
		env.current = &lateCurrent
		evaluatedBaseChanged, code, ok := env.refreshEvaluatedBaseSHA(mode)
		if !ok {
			return observation, code, false
		}
		if !mode.forced && (headChanged || baseChanged || evaluatedBaseChanged) {
			forceHeadRefresh = headChanged
			forceBaseRefresh = baseChanged || evaluatedBaseChanged || env.evaluatedBaseSHA != env.current.BaseSHA
			continue
		}
		return observation, 0, true
	}
	return observation, failProviderStage(
		env.stderr,
		fmt.Sprintf("stabilize pull request #%d", env.selectedNumber),
		fmt.Errorf("head or base changed during %d consecutive checkpoint reads", maxCheckpointRefreshes),
		"",
	), false
}

// noopGuardEscalation returns a terminal escalation reason when the implementer
// has reported no-work for the same head and cause(s) too many cycles running,
// or "" when the guard has not tripped.
func (env *remediationCheckpointRunEnv) noopGuardEscalation() (string, int, bool) {
	if env.features.NoopEscalationReason == nil {
		return "", 0, true
	}
	reason, err := env.features.NoopEscalationReason(
		env.root, env.selectedNumber, env.current, providerInput("remediationCauses", ""),
	)
	if err != nil {
		pf(env.stderr, "error: %v\n", err)
		return "", 1, false
	}
	return reason, 0, true
}

// priorCheckpointState reads back the sticky state this cycle advances, the
// comment ID it edits in place, and the human-comment watermark it records.
func (env *remediationCheckpointRunEnv) priorCheckpointState(comments []providers.Comment) (remediationState, string, string) {
	// Latest comment carrying an embedded payload wins, same rationale as
	// gather-pr-context's verdict scan: only the most recently recorded
	// checkpoint state is still actionable. Its comment ID (if any) is the
	// sticky comment this cycle edits in place (#716 AC3), rather than
	// posting a new one.
	prior, priorCommentID, _ := latestRemediationStateForPR(env.current.Body, comments)

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
	if prior.Escalated && !hasAnyLabel(env.current.Labels, []string{remediationEscalatedLabel}) {
		pf(env.stdout, "PR #%d: escalation cleared by an operator — resetting remediation budget (was %d cycles, attempts %s)\n",
			env.selectedNumber, prior.Cycles, renderRemediationAttempts(prior.AttemptsByCause))
		prior = remediationState{}
	}

	// #4058: the base-advance exit had the identical hole #1808 fixed one exit
	// over. escalationStillBlocks releases a rebase-curable park the moment the
	// live base tip moves past EscalatedBaseSHA, but the counters that escalated
	// it live in this payload, not in the label — so the re-admitted PR arrived
	// with failing-ci already at 2/2 and the very next cycle re-escalated with
	// budget-exhausted before an agent ever ran. LastDiffDigest does the same
	// through the stall check. The unpark was real but inert.
	//
	// A base advance is new evidence about exactly the causes
	// baseAdvanceCuresRemediationCause admits, so gate the reset on the same
	// predicate that authorized the release and drop the whole prior record,
	// matching the operator-clear reset above. A park whose cause a rebase
	// cannot cure never reaches here: escalationStillBlocks keeps it blocked.
	if prior.Escalated && escalationBaseAdvanceUnparks(prior) && prior.EscalatedBaseSHA != "" && env.features.LiveBaseTip != nil {
		liveBaseTip, err := env.features.LiveBaseTip(env.ctx, env.base)
		switch {
		case err != nil:
			// Fail closed to today's behavior: keep the prior record rather
			// than handing out a fresh budget on an unverified base advance.
			pf(env.stderr, "warning: could not resolve base branch %q tip for PR #%d (%v) — keeping the prior remediation budget\n",
				env.base, env.selectedNumber, err)
		case liveBaseTip != prior.EscalatedBaseSHA:
			pf(env.stdout, "PR #%d: escalation released by a base advance (%.12s -> %.12s) — resetting remediation budget (was %d cycles, attempts %s)\n",
				env.selectedNumber, prior.EscalatedBaseSHA, liveBaseTip,
				prior.Cycles, renderRemediationAttempts(prior.AttemptsByCause))
			prior = remediationState{}
		}
	}

	// Record the human-comment watermark this cycle sees. The refresh loop lists
	// comments unconditionally (even on forced escalations), so the newest
	// timestamp is always available; when the thread carries no timestamped
	// comment, carry the prior watermark forward rather than dropping it (which
	// would fail closed and never retrigger on human-comment again).
	watermark := latestCommentTimestamp(comments)
	if watermark == "" {
		watermark = prior.LastSeenCommentAt
	}
	return prior, priorCommentID, watermark
}

// externalRemediationBlock returns the park reason when this cycle is blocked
// only on state outside the PR's own diff (#2701), or "" when at least one
// detected cause is something an implementer in this worktree could fix.
//
// The base-branch check state is read only when a failing-ci cause is the last
// thing standing between this cycle and a park, so the ordinary remediation
// path costs no extra provider call. It fails OPEN — an unreadable base check
// state remediates as before rather than parking a PR on unverified evidence.
func (env *remediationCheckpointRunEnv) externalRemediationBlock(mode remediationCheckpointMode) string {
	if mode.forced || len(mode.causes) == 0 {
		return ""
	}
	if !externallyBlockedRemediationCauses(env.current.Labels, mode.causes, true) {
		return ""
	}
	if !slices.Contains(mode.causes, remediationCauseFailingCI) {
		return externalRemediationBlockReason(mode.causes, env.base)
	}
	if env.features.BaseCheckState == nil {
		return ""
	}
	baseState, err := env.features.BaseCheckState(env.ctx, env.base)
	if err != nil {
		pf(env.stderr, "warning: could not resolve check state for base branch %q of PR #%d (%v) — remediating the failing CI as usual\n",
			env.base, env.selectedNumber, err)
		return ""
	}
	if !externallyBlockedRemediationCauses(env.current.Labels, mode.causes, baseState == providers.CheckStateFailing) {
		return ""
	}
	return externalRemediationBlockReason(mode.causes, env.base)
}

func (env *remediationCheckpointRunEnv) escalate(
	decision remediationCheckpointDecision,
	mode remediationCheckpointMode,
	observation remediationCheckpointObservation,
	priorCommentID string,
) int {
	if env.features.SiblingOverlapContext != nil && !mode.forced && len(observation.structuralCollisions) == 0 {
		overlapContext, err := env.features.SiblingOverlapContext(
			env.ctx, env.selectedNumber, env.base, env.headPrefix, env.prs,
		)
		if err != nil {
			return failProviderStage(env.stderr, fmt.Sprintf("load sibling-overlap findings for PR #%d", env.selectedNumber), err, "")
		}
		decision.State.SiblingOverlapContext = overlapContext
	}
	if env.features.LiveBaseTip != nil {
		escalatedBaseTip, err := env.features.LiveBaseTip(env.ctx, env.base)
		if err != nil {
			return failProviderStage(env.stderr, fmt.Sprintf("resolve base branch %q tip for PR #%d", env.base, env.selectedNumber), err, "")
		}
		decision.State.EscalatedBaseSHA = escalatedBaseTip
	}
	if err := env.transport.UpdateLabels(env.ctx, []string{remediationEscalatedLabel}, []string{needsRemediationLabel}); err != nil {
		return failProviderStage(env.stderr, fmt.Sprintf("escalate PR #%d", env.selectedNumber), err, "")
	}
	if err := env.transport.PutStickyComment(env.ctx, priorCommentID, renderRemediationComment(decision.State)); err != nil {
		return failProviderStage(env.stderr, fmt.Sprintf("record escalation comment on PR #%d", env.selectedNumber), err, "")
	}
	if err := writeCheckpointResult(env.stderr, false, env.selectedNumber, env.current.Head, env.current.HeadSHA, decision.Escalation); err != nil {
		return 1
	}
	pf(env.stdout, "escalated PR #%d (escalation %d for head %s): %s\n", env.selectedNumber, decision.Escalation.Generation, env.current.HeadSHA, decision.Escalation.Reason)
	return 0
}

func (env *remediationCheckpointRunEnv) recordCheckpoint(decision remediationCheckpointDecision, digest, priorCommentID string) int {
	// Clear the label before replacing its escalation snapshot. Otherwise a
	// failed label removal leaves a non-escalated state that fails closed.
	if hasAnyLabel(env.current.Labels, []string{remediationEscalatedLabel}) {
		if err := env.transport.UpdateLabels(env.ctx, nil, []string{remediationEscalatedLabel}); err != nil {
			return failProviderStage(env.stderr, fmt.Sprintf("clear self-healed escalation label from PR #%d", env.selectedNumber), err, "")
		}
	}
	if err := env.transport.PutStickyComment(env.ctx, priorCommentID, renderRemediationComment(decision.State)); err != nil {
		return failProviderStage(env.stderr, fmt.Sprintf("record checkpoint state on PR #%d", env.selectedNumber), err, "")
	}
	if err := writeCheckpointResult(env.stderr, decision.HasObservedCause, env.selectedNumber, env.current.Head, env.current.HeadSHA, remediationEscalation{}); err != nil {
		return 1
	}

	if !decision.HasObservedCause {
		pf(
			env.stdout,
			"recorded no-cause checkpoint for PR #%d: cycle %d, counters unchanged, digest %s; remediation halted without consuming an allowance\n",
			env.selectedNumber, decision.State.Cycles, digest,
		)
		return 0
	}
	pf(
		env.stdout,
		"recorded checkpoint for PR #%d: cycle %d, attempts by cause %s, digest %s\n",
		env.selectedNumber, decision.State.Cycles, renderRemediationAttempts(decision.State.AttemptsByCause), digest,
	)
	return 0
}

func runRemediationCheckpoint(args []string, stdout, stderr io.Writer) int {
	flags, code, ok := parseRemediationCheckpointFlags(args, stderr)
	if !ok {
		return code
	}
	root := providerStageRoot(flags.pathArg)
	selectedNumber, code, ok := resolveCheckpointSelectedNumber(root, stderr)
	if !ok {
		return code
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	var (
		reader    remediationCheckpointReader
		transport remediationCheckpointTransport
		features  remediationCheckpointFeatures
	)
	if repo.Provider == providers.ProviderADO {
		provider, err := newProviderForStageAs[*providers.ADOProvider](root, repo, false, withStageProviderCapability(capability.GitHubPRWrite))
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		reader = provider
		transport = threadCheckpointTransport{provider: provider, repo: repo, pullID: strconv.Itoa(selectedNumber)}
		features, err = adoRemediationCheckpointFeatures()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	} else {
		token, err := providerToken(capability.GitHubPRWrite)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
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
		reader = provider
		transport = issueCommentCheckpointTransport{provider: provider, repo: repo, number: selectedNumber}
		features = githubRemediationCheckpointFeatures(provider, repo, pushToken)
	}

	return runRemediationCheckpointCore(
		root, repo, selectedNumber, flags, reader, transport, features, stdout, stderr,
	)
}

// runRemediationCheckpointCore owns the common checkpoint decision flow. Provider
// profiles contribute only transport and the enrichments they can safely perform.
func runRemediationCheckpointCore(
	root string,
	repo providers.RepositoryRef,
	selectedNumber int,
	flags remediationCheckpointFlags,
	reader remediationCheckpointReader,
	transport remediationCheckpointTransport,
	features remediationCheckpointFeatures,
	stdout, stderr io.Writer,
) int {
	ctx, cancel := providerCommandContext()
	defer cancel()

	env, code, ok := openRemediationCheckpointEnv(
		ctx, root, repo, selectedNumber, reader, transport, features, stdout, stderr,
	)
	if !ok {
		return code
	}
	mode, code, ok := resolveRemediationCheckpointMode(flags, features, stderr)
	if !ok {
		return code
	}
	if !mode.forced && sequencingOnlyCheckpointWait(env.current.Labels, mode.causes) {
		return env.recordSequencingOnlyWait()
	}
	if !mode.forced && mode.rebaseInfrastructureFailure {
		return env.recordInfrastructureNoop()
	}
	if !mode.forced {
		if env.features.CheckoutAuth == nil {
			pf(stderr, "error: checkpoint provider does not support a repository checkout\n")
			return 1
		}
		env.gitAuth = env.features.CheckoutAuth
		env.pushToken = env.features.CheckoutToken
	}
	if code, ok := env.resolveEvaluatedBaseSHA(mode); !ok {
		return code
	}
	observation, code, ok := env.stabilize(mode)
	if !ok {
		return code
	}
	if !mode.forced && len(mode.causes) > 0 {
		reason, code, ok := env.noopGuardEscalation()
		if !ok {
			return code
		}
		if reason != "" {
			mode.reason = reason
			mode.forced = true
		}
	}
	prior, priorCommentID, watermark := env.priorCheckpointState(observation.comments)
	pf(
		stdout,
		"PR #%d checkpoint base comparison: prior=%q, evaluated=%q (PR metadata=%q)\n",
		selectedNumber, prior.BaseSHA, env.evaluatedBaseSHA, env.current.BaseSHA,
	)
	externalBlockReason := env.externalRemediationBlock(mode)

	decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior: prior, Causes: mode.causes, Budgets: mode.budgets, Digest: observation.digest,
		HeadSHA: env.current.HeadSHA, BaseSHA: env.evaluatedBaseSHA, Watermark: watermark,
		Forced: mode.forced, ForcedReason: mode.reason, ForcedOutcome: mode.forcedOutcome,
		PolicyExcluded: mode.policyExcluded, StructuralCollisions: observation.structuralCollisions,
		StructuralCollisionContext: renderStructuralCollisionContext(selectedNumber, observation.structuralCollisions),
		ExternallyBlocked:          externalBlockReason != "",
		ExternalBlockReason:        externalBlockReason,
		RunID:                      strings.TrimSpace(os.Getenv(journalclient.EnvRunID)),
	})
	if decision.Escalated {
		return env.escalate(decision, mode, observation, priorCommentID)
	}
	return env.recordCheckpoint(decision, observation.digest, priorCommentID)
}

func remediationNoopEscalationReason(root string, selectedNumber int, current *providers.PullRequestSummary, rawCauses string) (string, error) {
	signature := remediationNoopSignature{
		HeadSHA: current.HeadSHA,
		Causes:  normalizeRemediationCauses(rawCauses),
	}
	l := layoutFor(root)
	record, err := remediationNoopRecordForSignature(l, selectedNumber, signature)
	if err != nil {
		return "", fmt.Errorf("inspect remediation no-op guard: %w", err)
	}
	gaggle := l.Gaggle()
	if gaggle == "" {
		gaggle = providerGaggle()
	}
	key := remediationNoopKey(gaggle, selectedNumber)
	if record.Parked && !hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
		if err := clearRemediationNoopRecord(l, key); err != nil {
			return "", fmt.Errorf("reset operator-cleared remediation no-op guard: %w", err)
		}
		record = remediationNoopRecord{}
	}
	if record.Attempts < remediationNoopLimit {
		return "", nil
	}
	if err := markRemediationNoopParked(l, key); err != nil {
		return "", fmt.Errorf("park remediation no-op guard: %w", err)
	}
	return fmt.Sprintf(
		"the implementer reported no-work %d consecutive times for unchanged head %s and remediation cause(s) %s",
		record.Attempts, signature.HeadSHA, signature.Causes,
	), nil
}

func postOrUpdateStickyThreadComment(ctx context.Context, provider *providers.ADOProvider, repo providers.RepositoryRef, pullID, existingCommentID, body string) error {
	if existingCommentID != "" {
		return provider.UpdatePullRequestThreadComment(ctx, repo, existingCommentID, body)
	}
	_, err := provider.PostPullRequestThreadComment(ctx, repo, pullID, body)
	return err
}

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

// defaultRemediationCauseBudget is the per-cause allowance a remediation
// cause gets when the workflow leaves its budget input unset (#2737). Every
// per-cause budget is a DEFAULT rather than a required input:
// declaredRemediationBudgets runs only once a cause fires, so a required input
// let a config validate, deploy and run clean for days and then fail a live PR
// the first time that cause appeared. `goobers validate` warns about each
// unset budget instead (providerstage.Input.UnsetDefault), so the policy
// choice stays visible to the author without breaking the stage.
const defaultRemediationCauseBudget = 2

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
		raw    string
		target *int
	}{
		{"conflictBudget", providerInput("conflictBudget", ""), &budgets.Conflict},
		{"substantiveBudget", providerInput("substantiveBudget", ""), &budgets.Substantive},
		{"failingCIBudget", providerInput("failingCIBudget", ""), &budgets.FailingCI},
		{"siblingOverlapBudget", providerInput("siblingOverlapBudget", ""), &budgets.SiblingOverlap},
		{"humanCommentBudget", providerInput("humanCommentBudget", ""), &budgets.HumanComment},
	}
	for _, value := range values {
		// An empty input falls back to the default, while a non-empty but
		// invalid value is still a hard error: a typo must not silently pick
		// up the default.
		if value.raw == "" {
			*value.target = defaultRemediationCauseBudget
			continue
		}
		budget, err := strconv.Atoi(value.raw)
		if err != nil || budget <= 0 {
			return remediationBudgets{}, fmt.Errorf("%s must be a positive integer, got %q", value.input, value.raw)
		}
		*value.target = budget
	}
	return budgets, nil
}

func exhaustedRemediationCause(attempts remediationAttempts, causes []remediationCause, budgets remediationBudgets) (remediationCause, bool) {
	for _, cause := range causes {
		if attempts.ForCause(cause) >= budgets.ForCause(cause) {
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

// externallyBlockedRemediationCauses reports whether EVERY cause this cycle
// detected is external to the PR's own diff (#2701), so no change an
// implementer could make in this PR's worktree resolves any of them.
//
// Two causes qualify, and only under evidence:
//
//   - sibling-overlap, when merge-review has already recorded the ordering as
//     goobers:blocked-on-sibling. Without that label the overlap is only a
//     finding, and adapting this PR to its sibling is ordinary in-worktree work.
//   - failing-ci, when required CI is red on the BASE BRANCH itself — the same
//     attribution #4058 established for releasing a failing-ci park: CI is
//     evaluated against merge(base, head), so a base that is red on its own is
//     not something this diff caused or can cure.
//
// Everything else — a rebase conflict, a substantive finding, a human comment —
// is in-worktree work by construction, so a single one of them makes the cycle
// actionable and the whole set is remediated as before. An empty cause set is
// likewise not an external block: recordCheckpoint's no-cause path already
// halts it without consuming an allowance.
func externallyBlockedRemediationCauses(labels []string, causes []remediationCause, baseCIFailing bool) bool {
	if len(causes) == 0 {
		return false
	}
	for _, cause := range causes {
		switch cause {
		case remediationCauseSiblingOverlap:
			if !hasAnyLabel(labels, []string{blockedOnSiblingLabel}) {
				return false
			}
		case remediationCauseFailingCI:
			if !baseCIFailing {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// externalRemediationBlockReason renders the human-facing account of an
// externally-blocked park for the sticky comment and the stage result.
func externalRemediationBlockReason(causes []remediationCause, baseBranch string) string {
	parts := make([]string, 0, len(causes))
	for _, cause := range remediationCauseOrder {
		if !slices.Contains(causes, cause) {
			continue
		}
		switch cause {
		case remediationCauseSiblingOverlap:
			parts = append(parts, fmt.Sprintf(
				"sibling sequencing merge-review already recorded as `%s`", blockedOnSiblingLabel))
		case remediationCauseFailingCI:
			parts = append(parts, fmt.Sprintf(
				"required CI is red on the base branch %q itself, so this PR's diff did not cause it", baseBranch))
		}
	}
	return fmt.Sprintf(
		"every detected remediation cause is external to this PR's own diff (%s) — parked without spending a remediation pass, since no change inside this PR can resolve it",
		strings.Join(parts, "; "),
	)
}

func renderRemediationAttempts(attempts remediationAttempts) string {
	parts := make([]string, 0, len(remediationCauseOrder))
	for _, cause := range remediationCauseOrder {
		parts = append(parts, fmt.Sprintf("%s=%d", cause, attempts.ForCause(cause)))
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
		if attempts.ForCause(cause) > 0 {
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
	cmd := workspaceGitCommand(dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := workspaceGitOutput(cmd)
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
	cmd := workspaceGitCommand(dir, "diff", baseSHA+"...HEAD")
	out, err := workspaceGitOutput(cmd)
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

// The pure remediation selection lives in internal/remediationselect (#8131).
// These re-export it under the spellings this package has always used, so the
// stages, the schedule-demand counter and their tests are unchanged.
const blockedOnSiblingLabel = remediationselect.BlockedOnSiblingLabel
const mergeDemotedLabel = remediationselect.MergeDemotedLabel
const mergeReviewStatusMarker = remediationselect.MergeReviewStatusMarker
const needsRemediationLabel = remediationselect.NeedsRemediationLabel
const pullRequestClaimPrefix = remediationselect.PullRequestClaimPrefix
const remediateCauseBehindBase = remediationselect.RemediateCauseBehindBase
const remediateCauseConflict = remediationselect.RemediateCauseConflict
const remediateCauseFailingCI = remediationselect.RemediateCauseFailingCI
const remediateCauseHumanComment = remediationselect.RemediateCauseHumanComment
const remediateCauseSiblingOverlap = remediationselect.RemediateCauseSiblingOverlap
const remediateCauseSubstantive = remediationselect.RemediateCauseSubstantive
const remediationCauseConflict = remediationselect.RemediationCauseConflict
const remediationCauseFailingCI = remediationselect.RemediationCauseFailingCI
const remediationCauseHumanComment = remediationselect.RemediationCauseHumanComment
const remediationCauseSiblingOverlap = remediationselect.RemediationCauseSiblingOverlap
const remediationCauseSubstantive = remediationselect.RemediationCauseSubstantive
const remediationEscalatedLabel = remediationselect.RemediationEscalatedLabel
const remediationOutcomeBudgetExhausted = remediationselect.RemediationOutcomeBudgetExhausted
const remediationOutcomeDidNotConverge = remediationselect.RemediationOutcomeDidNotConverge
const remediationOutcomeExternallyBlocked = remediationselect.RemediationOutcomeExternallyBlocked
const remediationOutcomeInfrastructure = remediationselect.RemediationOutcomeInfrastructure
const remediationOutcomePolicyExcluded = remediationselect.RemediationOutcomePolicyExcluded
const remediationPriorityBehindBase = remediationselect.RemediationPriorityBehindBase
const remediationPriorityFailingCI = remediationselect.RemediationPriorityFailingCI
const remediationPriorityNeedsRemediation = remediationselect.RemediationPriorityNeedsRemediation
const remediationPriorityNone = remediationselect.RemediationPriorityNone

func demotionStillHolds(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	return remediationselect.DemotionStillHolds(ctx, provider, repo, pr)
}
func escalationStillBlocks(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	return remediationselect.EscalationStillBlocks(ctx, provider, repo, pr)
}
func filterLiveBlockedOnSiblingBlockers(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blockers []int) ([]int, error) {
	return remediationselect.FilterLiveBlockedOnSiblingBlockers(ctx, provider, repo, blockers)
}
func filterRemediationPullRequests(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary, heldBranches map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
	return remediationselect.FilterRemediationPullRequests(ctx, provider, repo, prs, heldBranches)
}
func namedBlockerStillBlocks(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, blocker int) (bool, error) {
	return remediationselect.NamedBlockerStillBlocks(ctx, provider, repo, blocker)
}
func pullRequestBehindLiveBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (bool, error) {
	return remediationselect.PullRequestBehindLiveBase(ctx, provider, repo, pr, baseTips)
}
func pullRequestLiveBaseMergeBase(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (mergeBase string, baseTip string, err error) {
	return remediationselect.PullRequestLiveBaseMergeBase(ctx, provider, repo, pr, baseTips)
}

type blockedOnSiblingState = remediationselect.BlockedOnSiblingState
type implementationEscalationState = remediationselect.ImplementationEscalationState
type mergeDemotionState = remediationselect.MergeDemotionState
type operatorRetryRecord = remediationselect.OperatorRetryRecord
type remediationAttempts = remediationselect.RemediationAttempts
type remediationBudgets = remediationselect.RemediationBudgets
type remediationCause = remediationselect.RemediationCause
type remediationEscalationOutcome = remediationselect.RemediationEscalationOutcome
type remediationPriority = remediationselect.RemediationPriority
type remediationState = remediationselect.RemediationState
type siblingOverlapFinding = remediationselect.SiblingOverlapFinding

var behindBaseRemediationCandidates = remediationselect.BehindBaseRemediationCandidates
var escalationBaseAdvanceUnparks = remediationselect.EscalationBaseAdvanceUnparks
var escalationCausesAreOrderingOnly = remediationselect.EscalationCausesAreOrderingOnly
var filterLiveOriginPullRequests = remediationselect.FilterLiveOriginPullRequests
var hasAnyLabel = remediationselect.HasAnyLabel
var implementationEscalationPattern = remediationselect.ImplementationEscalationPattern
var isMergeReviewStatusComment = remediationselect.IsMergeReviewStatusComment
var latestBlockedOnSiblingState = remediationselect.LatestBlockedOnSiblingState
var latestMergeDemotionState = remediationselect.LatestMergeDemotionState
var latestRemediationState = remediationselect.LatestRemediationState
var latestRemediationStateForPR = remediationselect.LatestRemediationStateForPR
var parseBlockedOnSiblingComment = remediationselect.ParseBlockedOnSiblingComment
var parseMergeDemotionComment = remediationselect.ParseMergeDemotionComment
var parseRemediationStateComment = remediationselect.ParseRemediationStateComment
var parseVerdictComment = remediationselect.ParseVerdictComment
var prReferencePattern = remediationselect.PrReferencePattern
var pullRequestClaimKey = remediationselect.PullRequestClaimKey
var pullRequestClaimLedgerKey = remediationselect.PullRequestClaimLedgerKey
var pullRequestClaimStatus = remediationselect.PullRequestClaimStatus
var pullRequestClaimStatusFor = remediationselect.PullRequestClaimStatusFor
var remediationCauseOrder = remediationselect.RemediationCauseOrder
var remediationPriorityFor = remediationselect.RemediationPriorityFor
var resolvePullRequestClaim = remediationselect.ResolvePullRequestClaim
var runBranchOriginRunID = remediationselect.RunBranchOriginRunID
var selectRemediationCandidates = remediationselect.SelectRemediationCandidates
var shouldParkRemediation = remediationselect.ShouldParkRemediation
var strongRemediationCandidates = remediationselect.StrongRemediationCandidates
var substantiveFindingAppliesToPR = remediationselect.SubstantiveFindingAppliesToPR
var unionBlockingPRs = remediationselect.UnionBlockingPRs
var verdictHasSubstantiveFindingForPR = remediationselect.VerdictHasSubstantiveFindingForPR
var verdictJSONPattern = remediationselect.VerdictJSONPattern

func pullRequestClaimListing(ledger claimsclient.Ledger, gaggle string, provider providers.ProviderKind) (claimsclient.Listing, error) {
	return remediationselect.PullRequestClaimListingContext(claimContext(), ledger, gaggle, provider)
}
