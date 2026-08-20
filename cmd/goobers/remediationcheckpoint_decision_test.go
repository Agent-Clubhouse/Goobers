package main

import (
	"strings"
	"testing"
)

func TestDecideRemediationCheckpoint(t *testing.T) {
	t.Run("records an advancing cycle", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   remediationState{Cycles: 2, AttemptsByCause: remediationAttempts{Conflict: 1}},
			Causes:  []remediationCause{remediationCauseSubstantive},
			Budgets: remediationBudgets{Substantive: 2},
			Digest:  "sha256:new",
			HeadSHA: "head",
			BaseSHA: "base",
		})

		if got.Escalated || !got.HasObservedCause {
			t.Fatalf("decision = %+v, want an advancing observed-cause checkpoint", got)
		}
		if got.State.Cycles != 3 || got.State.AttemptsByCause.Conflict != 1 || got.State.AttemptsByCause.Substantive != 1 {
			t.Fatalf("state = %+v, want cycle 3 with independent cause counters", got.State)
		}
	})

	t.Run("budget exhaustion escalates", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   remediationState{AttemptsByCause: remediationAttempts{Conflict: 2}},
			Causes:  []remediationCause{remediationCauseConflict},
			Budgets: remediationBudgets{Conflict: 2},
			Digest:  "sha256:new",
			HeadSHA: "head",
			BaseSHA: "base",
		})

		if !got.Escalated || got.Escalation.Outcome != remediationOutcomeBudgetExhausted {
			t.Fatalf("decision = %+v, want budget-exhausted escalation", got)
		}
		if !strings.Contains(got.Escalation.Reason, `cause "conflict" exhausted its budget`) {
			t.Fatalf("reason = %q, want cause-specific budget reason", got.Escalation.Reason)
		}
	})

	t.Run("same digest on same base escalates", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:   remediationState{LastDiffDigest: "sha256:same", BaseSHA: "base"},
			Digest:  "sha256:same",
			HeadSHA: "head",
			BaseSHA: "base",
		})

		if !got.Escalated || !strings.Contains(got.Escalation.Reason, "byte-identical") {
			t.Fatalf("decision = %+v, want same-diff escalation", got)
		}
	})

	t.Run("forced policy exclusion takes precedence", func(t *testing.T) {
		got := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
			Prior:          remediationState{AttemptsByCause: remediationAttempts{Conflict: 2}},
			Causes:         []remediationCause{remediationCauseConflict},
			Budgets:        remediationBudgets{Conflict: 2},
			Digest:         "sha256:same",
			HeadSHA:        "head",
			BaseSHA:        "base",
			Forced:         true,
			ForcedReason:   "policy excludes this repair",
			ForcedOutcome:  remediationOutcomeDidNotConverge,
			PolicyExcluded: true,
		})

		if !got.Escalated || got.Escalation.Outcome != remediationOutcomePolicyExcluded ||
			got.Escalation.Reason != "policy excludes this repair" || got.Escalation.Attempted {
			t.Fatalf("decision = %+v, want policy-excluded forced escalation to win", got)
		}
		if len(got.State.EscalationCauses) != 0 {
			t.Fatalf("escalation causes = %v, want none for a forced escalation", got.State.EscalationCauses)
		}
	})
}
