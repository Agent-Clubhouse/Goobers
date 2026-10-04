package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// TestDecideRemediationCheckpointSettlesInfrastructureVoidedCycle pins the
// #5588/#5598 decision: a prior cycle whose run the failed-terminal handler
// voided on infrastructure is refunded and never read as a stalled attempt,
// while an ordinary prior cycle keeps every existing escalation.
func TestDecideRemediationCheckpointSettlesInfrastructureVoidedCycle(t *testing.T) {
	voided := func(attempts remediationAttempts, infraFailures int) remediationState {
		return remediationState{
			Cycles: 2, AttemptsByCause: attempts,
			LastDiffDigest: "sha256:same", HeadSHA: "head", BaseSHA: "base",
			RunID: "run-prior", ChargedCauses: []remediationCause{remediationCauseFailingCI},
			InfrastructureVoided: true, InfrastructureFailures: infraFailures,
		}
	}

	t.Run("#5588 a voided charge is refunded before the budget check", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   voided(remediationAttempts{FailingCI: 2}, 0),
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:new", HeadSHA: "head", BaseSHA: "base",
			RunID: "run-now",
		})
		if got.Escalated {
			t.Fatalf("decision = %+v, want the voided attempt refunded, not budget-exhausted", got.Escalation)
		}
		if got.State.AttemptsByCause.FailingCI != 2 {
			t.Fatalf("failing-ci attempts = %d, want 2 (refund one, charge this cycle)", got.State.AttemptsByCause.FailingCI)
		}
		if got.State.InfrastructureFailures != 1 || got.State.RunID != "run-now" ||
			len(got.State.ChargedCauses) != 1 || got.State.ChargedCauses[0] != remediationCauseFailingCI {
			t.Fatalf("state = %+v, want this cycle's provisional charge and one consecutive voided cycle", got.State)
		}
	})

	t.Run("#5598 a voided cycle's unchanged diff is not did-not-converge", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   voided(remediationAttempts{FailingCI: 1}, 0),
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:same", HeadSHA: "head", BaseSHA: "base",
		})
		if got.Escalated {
			t.Fatalf("decision = %+v, want no same-diff escalation against a cycle whose agent never ran", got.Escalation)
		}
	})

	t.Run("an unvoided identical cycle still escalates", func(t *testing.T) {
		prior := voided(remediationAttempts{FailingCI: 1}, 0)
		prior.InfrastructureVoided = false
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   prior,
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:same", HeadSHA: "head", BaseSHA: "base",
		})
		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeDidNotConverge {
			t.Fatalf("decision = %+v, want did-not-converge for a settled identical cycle", got)
		}
		if got.State.InfrastructureFailures != 0 {
			t.Fatalf("infrastructure failures = %d, want the streak reset", got.State.InfrastructureFailures)
		}
	})

	t.Run("an unvoided exhausted budget still escalates", func(t *testing.T) {
		prior := voided(remediationAttempts{FailingCI: 2}, 0)
		prior.InfrastructureVoided = false
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   prior,
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:new", HeadSHA: "head", BaseSHA: "base",
		})
		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeBudgetExhausted {
			t.Fatalf("decision = %+v, want budget-exhausted", got)
		}
	})

	t.Run("the infrastructure allowance bounds the refund", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   voided(remediationAttempts{FailingCI: 1}, remediationInfrastructureAllowance-1),
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:same", HeadSHA: "head", BaseSHA: "base",
		})
		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeInfrastructure {
			t.Fatalf("decision = %+v, want an infrastructure-failure park after %d voided cycles", got, remediationInfrastructureAllowance)
		}
		if !strings.Contains(got.Escalation.Reason, "infrastructure failure") {
			t.Fatalf("reason = %q", got.Escalation.Reason)
		}
	})
}

// remediationVoidFixture seeds a repo-backed instance whose run holds a claim
// on PR #77 and returns the wired failed handler plus the fake provider
// carrying the PR's remediation-state comment.
func remediationVoidFixture(t *testing.T, runID string, state remediationState) (runner.FailedHandler, *blockedHandlerFakeCommenter) {
	t.Helper()
	fake := &blockedHandlerFakeCommenter{comments: []providers.Comment{
		{ID: "c1", Body: "an unrelated review comment"},
		{ID: "c2", Body: renderRemediationComment(state)},
	}}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("77", runID, "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	annotations, err := openStageAnnotator(l)
	if err != nil {
		t.Fatalf("openStageAnnotator: %v", err)
	}
	if err := recordItemRepository(annotations, runID, "77", itemKindPullRequest,
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}); err != nil {
		t.Fatalf("recordItemRepository: %v", err)
	}
	if err := annotations.Close(); err != nil {
		t.Fatalf("close annotator: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
	if h == nil {
		t.Fatal("expected a non-nil handler for a repo-backed instance")
	}
	return h, fake
}

// TestFailedHandlerVoidsRemediationChargeOnInfrastructureTerminal is the
// settle half of #5588/#5598: a pr-remediation run that recorded a charged
// checkpoint and then died on infrastructure marks that record voided, so the
// next checkpoint refunds it. A work failure, or a record another run wrote,
// is left exactly as it was.
func TestFailedHandlerVoidsRemediationChargeOnInfrastructureTerminal(t *testing.T) {
	const runID = "run-remediation"
	charged := remediationState{
		Cycles: 1, AttemptsByCause: remediationAttempts{FailingCI: 1},
		LastDiffDigest: "sha256:d", HeadSHA: "head", BaseSHA: "base",
		RunID: runID, ChargedCauses: []remediationCause{remediationCauseFailingCI},
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}

	for _, tc := range []struct {
		name       string
		state      remediationState
		outcome    runner.FailedOutcome
		wantVoided bool
	}{
		{
			name:       "harness preflight never reached an agent turn",
			state:      charged,
			outcome:    runner.FailedOutcome{RunID: runID, RepoRef: repo, Stage: "implement", Code: telemetry.ErrCodeAgenticExecutorUnavailable},
			wantVoided: true,
		},
		{
			name:       "explicit infra class from an exhausted infrastructure budget",
			state:      charged,
			outcome:    runner.FailedOutcome{RunID: runID, RepoRef: repo, Stage: "implement", Code: "run_failed", FaultClass: telemetry.ErrorClassInfra},
			wantVoided: true,
		},
		{
			name:    "work failure keeps the charge",
			state:   charged,
			outcome: runner.FailedOutcome{RunID: runID, RepoRef: repo, Stage: "implement", Code: "nonzero_exit"},
		},
		{
			name: "another run's record is not this run's to void",
			state: func() remediationState {
				s := charged
				s.RunID = "run-other"
				return s
			}(),
			outcome: runner.FailedOutcome{RunID: runID, RepoRef: repo, Stage: "implement", Code: telemetry.ErrCodeAgenticExecutorUnavailable},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := remediationVoidFixture(t, runID, tc.state)
			if err := h(context.Background(), tc.outcome); err != nil {
				t.Fatalf("handler: %v", err)
			}
			got, _, found := latestRemediationState(fake.comments)
			if !found {
				t.Fatal("remediation-state comment disappeared")
			}
			if got.InfrastructureVoided != tc.wantVoided {
				t.Fatalf("InfrastructureVoided = %v, want %v (state %+v)", got.InfrastructureVoided, tc.wantVoided, got)
			}
			if tc.wantVoided && (got.AttemptsByCause.FailingCI != 1 || got.LastDiffDigest != "sha256:d") {
				t.Fatalf("voiding rewrote the recorded charge: %+v", got)
			}
		})
	}
}

// TestDecideRemediationCheckpointRetryOfOwnWriteIsIdempotent pins #6008: a
// stage retry that reads back the state comment its own run wrote for the
// same head and base is not a no-progress repeat and does not charge twice.
func TestDecideRemediationCheckpointRetryOfOwnWriteIsIdempotent(t *testing.T) {
	first := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior:   remediationState{Cycles: 1, AttemptsByCause: remediationAttempts{FailingCI: 1}, LastDiffDigest: "sha256:older", HeadSHA: "head", BaseSHA: "base"},
		Causes:  []remediationCause{remediationCauseFailingCI},
		Budgets: remediationBudgets{FailingCI: 2},
		Digest:  "sha256:now", HeadSHA: "head", BaseSHA: "base", RunID: "run-now",
	})
	if first.Escalated || first.State.AttemptsByCause.FailingCI != 2 {
		t.Fatalf("first attempt = %+v, want an advancing cycle charging failing-ci to 2/2", first)
	}

	retry := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior:   first.State,
		Causes:  []remediationCause{remediationCauseFailingCI},
		Budgets: remediationBudgets{FailingCI: 2},
		Digest:  "sha256:now", HeadSHA: "head", BaseSHA: "base", RunID: "run-now",
	})
	if retry.Escalated {
		t.Fatalf("retry of the run's own write escalated: %+v", retry.Escalation)
	}
	if retry.State.AttemptsByCause != first.State.AttemptsByCause || retry.State.Cycles != first.State.Cycles ||
		retry.State.LastDiffDigest != first.State.LastDiffDigest {
		t.Fatalf("retry state = %+v, want the first attempt's state %+v reproduced", retry.State, first.State)
	}

	t.Run("a different run on the same head and diff still stalls", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   first.State,
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 3},
			Digest:  "sha256:now", HeadSHA: "head", BaseSHA: "base", RunID: "run-next",
		})
		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeDidNotConverge {
			t.Fatalf("decision = %+v, want did-not-converge across remediation attempts", got.Escalation)
		}
	})

	t.Run("the same run after the head moved is judged normally", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   first.State,
			Causes:  []remediationCause{remediationCauseFailingCI},
			Budgets: remediationBudgets{FailingCI: 2},
			Digest:  "sha256:next", HeadSHA: "head-2", BaseSHA: "base", RunID: "run-now",
		})
		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeBudgetExhausted {
			t.Fatalf("decision = %+v, want budget-exhausted once the head moved", got.Escalation)
		}
	})
}
