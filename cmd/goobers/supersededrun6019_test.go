package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/providers"
)

// TestWorkspaceProvisionFailureCodeTypesAMovedPullRequestHead pins the pod
// half of #6019: the ancestry guard's refusal on a REBOUND branch (a pull
// request head someone pushed to while the run worked) surrenders its own
// code, which is what lets the engine end the run superseded instead of
// failed. A divergence on the run's own branch, or any other provisioning
// fault, keeps the generic infrastructure code.
func TestWorkspaceProvisionFailureCodeTypesAMovedPullRequestHead(t *testing.T) {
	divergedFrom := func(origin string) error {
		return fmt.Errorf("workspace delta: %w (the workspace is on branch %q)",
			&workspacedelta.DivergedError{Digest: "sha256:d", Current: "aaaa", Tip: "bbbb", Origin: origin}, "goobers/impl/pr")
	}
	diverged := divergedFrom("cccc")

	t.Setenv(dispatcher.EnvWorkspaceBranch, "goobers/impl/pr")
	if got := workspaceProvisionFailureCode(diverged); got != runsemantics.WorkspaceHeadMovedErrorCode {
		t.Fatalf("rebound divergence from a moved head code = %q, want %q", got, runsemantics.WorkspaceHeadMovedErrorCode)
	}
	if got := workspaceProvisionFailureCode(divergedFrom("AAAA")); got != telemetry.ErrCodeWorkspaceProvisionFailed {
		t.Fatalf("divergence on an unmoved head code = %q, want %q: the run rewrote its own history", got, telemetry.ErrCodeWorkspaceProvisionFailed)
	}
	if got := workspaceProvisionFailureCode(divergedFrom("")); got != telemetry.ErrCodeWorkspaceProvisionFailed {
		t.Fatalf("divergence with no recorded origin code = %q, want %q: a move that cannot be established is not typed", got, telemetry.ErrCodeWorkspaceProvisionFailed)
	}
	if got := workspaceProvisionFailureCode(errors.New("clone failed")); got != telemetry.ErrCodeWorkspaceProvisionFailed {
		t.Fatalf("clone failure code = %q, want %q", got, telemetry.ErrCodeWorkspaceProvisionFailed)
	}

	t.Setenv(dispatcher.EnvWorkspaceBranch, "")
	if got := workspaceProvisionFailureCode(diverged); got != telemetry.ErrCodeWorkspaceProvisionFailed {
		t.Fatalf("run-branch divergence code = %q, want %q: the run branch moving is a fault, not someone's push", got, telemetry.ErrCodeWorkspaceProvisionFailed)
	}
}

// TestWorkspaceHeadMovedCodeIsAnItemJudgment keeps the two spellings of the
// code in step and pins its class: if a superseded run ever does end failed
// (a workflow shape the engine's typed exit does not cover), the failure
// streak must still leave the moved pull request alone.
func TestWorkspaceHeadMovedCodeIsAnItemJudgment(t *testing.T) {
	if telemetry.ErrCodeWorkspaceHeadMoved != runsemantics.WorkspaceHeadMovedErrorCode {
		t.Fatalf("telemetry code %q != runner code %q", telemetry.ErrCodeWorkspaceHeadMoved, runsemantics.WorkspaceHeadMovedErrorCode)
	}
	if got := telemetry.ClassifyError(runsemantics.WorkspaceHeadMovedErrorCode); got != telemetry.ErrorClassItemJudgment {
		t.Fatalf("class = %q, want %q", got, telemetry.ErrorClassItemJudgment)
	}
}

const supersededRunID = "run-superseded"

var supersededRunRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

// seedSupersededRun builds an instance whose run recorded PR #77's identity,
// optionally still holds its claim, and journaled events; it returns the
// layout and a commenter carrying the PR's charged remediation-state comment.
func seedSupersededRun(t *testing.T, holdClaim bool, events ...journal.Event) (instance.Layout, *blockedHandlerFakeCommenter) {
	t.Helper()
	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	itemID := pullRequestClaimKey(77)
	if holdClaim {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			t.Fatalf("OpenClaimLedger: %v", err)
		}
		if ok, _, err := ledger.Claim(itemID, supersededRunID, "pr-remediation", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
	}
	annotations, err := openStageAnnotator(l)
	if err != nil {
		t.Fatalf("openStageAnnotator: %v", err)
	}
	if err := recordItemRepository(annotations, supersededRunID, itemID, itemKindPullRequest, supersededRunRepo); err != nil {
		t.Fatalf("recordItemRepository: %v", err)
	}
	if err := annotations.Close(); err != nil {
		t.Fatalf("close annotator: %v", err)
	}

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: supersededRunID, Workflow: "pr-remediation"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	for _, event := range events {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	charged := remediationState{
		Cycles: 1, AttemptsByCause: remediationAttempts{FailingCI: 1},
		LastDiffDigest: "sha256:d", HeadSHA: "head", BaseSHA: "base",
		RunID: supersededRunID, ChargedCauses: []remediationCause{remediationCauseFailingCI},
	}
	return l, &blockedHandlerFakeCommenter{comments: []providers.Comment{
		{ID: "c1", Body: "an unrelated review comment"},
		{ID: "c2", Body: renderRemediationComment(charged)},
	}}
}

func headMovedEvent(stage string) journal.Event {
	return journal.Event{
		Type: journal.EventError, Stage: stage,
		Error: &journal.ErrorDetail{Code: runsemantics.WorkspaceHeadMovedErrorCode, Message: "ancestry guard refused the delta"},
	}
}

func staleSelectionEvent(stage string) journal.Event {
	return journal.Event{
		Type: journal.EventStageFinished, Stage: stage, Status: string(apiv1.ResultNoWork),
		Outputs: map[string]any{"outcome": prClaimOutcomeStaleSelection, "selectedNumber": "77", "noWork": true},
	}
}

// TestSettleSupersededRunRefundsTheMovedPullRequestsCharge is the terminal
// half of #6019: a run that stopped because its pull request moved under it
// is reported superseded — so the policy leaves the item's failure and no-work
// streaks alone — and the remediation attempt it charged is marked for
// refund, whether the stop was the engine's typed head-moved exit (claim
// still held) or a pr-claim guard's stale selection (claim already released).
func TestSettleSupersededRunRefundsTheMovedPullRequestsCharge(t *testing.T) {
	for _, tc := range []struct {
		name           string
		holdClaim      bool
		finalState     string
		events         []journal.Event
		wantSuperseded bool
		wantRefund     bool
	}{
		{
			name: "reviewer pod refused a moved head", holdClaim: true, finalState: "review",
			events:         []journal.Event{headMovedEvent("review")},
			wantSuperseded: true, wantRefund: true,
		},
		{
			name: "guard found a stale selection after releasing the claim", finalState: "guard-before-review",
			events:         []journal.Event{staleSelectionEvent("guard-before-review")},
			wantSuperseded: true, wantRefund: true,
		},
		{
			name: "a run that already published keeps its charge", finalState: "guard-before-review",
			events: []journal.Event{
				{Type: journal.EventStageFinished, Stage: prPublicationStage, Status: string(apiv1.ResultSuccess),
					Outputs: map[string]any{pushRemediatedPublishedOutput: "true", pushRemediatedLocalHeadOutput: "cafe"}},
				staleSelectionEvent("guard-before-review"),
			},
			wantSuperseded: true,
		},
		{
			name: "an earlier stage's head-moved marker is not the run's terminal", holdClaim: true, finalState: "release-claim",
			events: []journal.Event{headMovedEvent("review"),
				{Type: journal.EventStageFinished, Stage: "release-claim", Status: string(apiv1.ResultSuccess)}},
		},
		{
			name: "an ordinary completion is not superseded", holdClaim: true, finalState: "release-claim",
			events: []journal.Event{{Type: journal.EventStageFinished, Stage: "release-claim", Status: string(apiv1.ResultSuccess)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, fake := seedSupersededRun(t, tc.holdClaim, tc.events...)
			superseded, err := settleSupersededRun(context.Background(), fake, l, supersededRunID, tc.finalState)
			if err != nil {
				t.Fatalf("settleSupersededRun: %v", err)
			}
			if superseded != tc.wantSuperseded {
				t.Fatalf("superseded = %v, want %v", superseded, tc.wantSuperseded)
			}
			got, _, found := latestRemediationState(fake.comments)
			if !found {
				t.Fatal("remediation-state comment disappeared")
			}
			if got.Superseded != tc.wantRefund {
				t.Fatalf("Superseded = %v, want %v (state %+v)", got.Superseded, tc.wantRefund, got)
			}
			if got.InfrastructureVoided {
				t.Fatalf("a superseded run was marked an infrastructure void: %+v", got)
			}
		})
	}
}

// TestDecideRemediationCheckpointRefundsSupersededCycle pins the checkpoint
// half: the next cycle refunds a superseded cycle's charge and ignores its
// digest, and — unlike an infrastructure void — does not count it toward the
// infrastructure allowance.
func TestDecideRemediationCheckpointRefundsSupersededCycle(t *testing.T) {
	prior := remediationState{
		Cycles: 2, AttemptsByCause: remediationAttempts{FailingCI: 2},
		LastDiffDigest: "sha256:same", HeadSHA: "head", BaseSHA: "base",
		RunID: "run-prior", ChargedCauses: []remediationCause{remediationCauseFailingCI},
		Superseded: true, InfrastructureFailures: 1,
	}
	got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior:   prior,
		Causes:  []remediationCause{remediationCauseFailingCI},
		Budgets: remediationBudgets{FailingCI: 2},
		Digest:  "sha256:same", HeadSHA: "head", BaseSHA: "base",
		RunID: "run-now",
	})
	if got.Escalated {
		t.Fatalf("decision = %+v, want the superseded attempt refunded, not budget-exhausted or did-not-converge", got.Escalation)
	}
	if got.State.AttemptsByCause.FailingCI != 2 {
		t.Fatalf("failing-ci attempts = %d, want 2 (refund the superseded one, charge this cycle)", got.State.AttemptsByCause.FailingCI)
	}
	if got.State.InfrastructureFailures != 1 {
		t.Fatalf("infrastructure failures = %d, want 1 carried unchanged — a push is not an infrastructure failure", got.State.InfrastructureFailures)
	}
	if got.State.Superseded {
		t.Fatal("the new cycle inherited the superseded mark")
	}
	if got.State.SupersededCycles != 1 {
		t.Fatalf("superseded cycles = %d, want 1 carried forward", got.State.SupersededCycles)
	}

	// Review finding on #7403: a head that moves under every cycle must not
	// be remediated forever with every cycle refunded.
	t.Run("the superseded allowance bounds the refund", func(t *testing.T) {
		bounded := prior
		bounded.SupersededCycles = remediationSupersededAllowance - 1
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   bounded,
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:new", HeadSHA: "head2", BaseSHA: "base",
			RunID: "run-now",
		})
		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeDidNotConverge ||
			!strings.Contains(got.Escalation.Reason, "head moved under each of the last") {
			t.Fatalf("decision = %+v, want a did-not-converge park after %d superseded cycles", got.Escalation, remediationSupersededAllowance)
		}
	})

	t.Run("an ordinary cycle resets the count", func(t *testing.T) {
		ordinary := prior
		ordinary.Superseded = false
		ordinary.SupersededCycles = 2
		ordinary.AttemptsByCause = remediationAttempts{FailingCI: 1}
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   ordinary,
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 3},
			Digest:  "sha256:new", HeadSHA: "head2", BaseSHA: "base",
			RunID: "run-now",
		})
		if got.Escalated || got.State.SupersededCycles != 0 {
			t.Fatalf("decision = %+v state = %+v, want an advancing cycle with the count reset", got.Escalation, got.State)
		}
	})
}

// TestSettleSupersededRunFailsClosedOnAnUnreadableJournal pins the review
// finding: a run whose journal cannot be read is an error, never an ordinary
// completion that resets the item's streaks.
func TestSettleSupersededRunFailsClosedOnAnUnreadableJournal(t *testing.T) {
	l, fake := seedSupersededRun(t, true, headMovedEvent("review"))
	// A journal that exists but cannot be read: its event log is not JSON.
	if err := os.WriteFile(filepath.Join(l.RunsDir(), supersededRunID, "events.jsonl"), []byte("{not json\n{also not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	superseded, err := settleSupersededRun(context.Background(), fake, l, supersededRunID, "review")
	if err == nil || superseded {
		t.Fatalf("superseded = %v, err = %v; want an error for a journal that cannot be read", superseded, err)
	}

	// No run directory at all is not a superseded run: there is nothing it
	// could have journaled.
	none := instance.NewLayout(t.TempDir())
	if superseded, err := settleSupersededRun(context.Background(), fake, none, "run-missing", "review"); err != nil || superseded {
		t.Fatalf("missing run: superseded = %v, err = %v; want an ordinary completion", superseded, err)
	}
}
