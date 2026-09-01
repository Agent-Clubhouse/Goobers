package main

import (
	"reflect"
	"testing"
)

// TestForcedEscalationAttributesItsCause covers #4074. A forced escalation
// means the in-run reviewer returned a terminal fail on the remediation
// ATTEMPT, not on the PR. Discarding the cause made escalationBaseAdvanceUnparks
// read the record as "no remediation cause was ever observed" and refuse every
// base advance forever — and pr-remediation excludes escalated PRs upstream, so
// the head could never move either, leaving the exit the escalation comment
// advertises unreachable.
//
// Live on 2026-08-31 that was four of the six PRs in the lane. #3894 and #3891
// were each displaced by a sibling merge, attempted exactly one thing — a
// rebase (`attemptedCauses: ["conflict"]`) — had that rebase rejected, and sat
// unreachable for a day while the base advanced past their pinned snapshots.
func TestForcedEscalationAttributesItsCause(t *testing.T) {
	tests := []struct {
		name           string
		priorAttempts  remediationAttempts
		forcedOutcome  remediationEscalationOutcome
		policyExcluded bool
		wantCauses     []remediationCause
		wantUnparks    bool
	}{
		{
			// #3894 / #3891: displaced by a sibling merge, one rebase
			// attempted, the reviewer rejected the resolution. A conflict
			// resolution is a function of the base, so the next base advance
			// produces a different one.
			name:          "a rejected rebase is cured by the next base advance",
			priorAttempts: remediationAttempts{Conflict: 1},
			forcedOutcome: remediationOutcomeDidNotConverge,
			wantCauses:    []remediationCause{remediationCauseConflict},
			wantUnparks:   true,
		},
		{
			// #3900.
			name:          "conflict plus sibling-overlap is entirely rebase-curable",
			priorAttempts: remediationAttempts{Conflict: 1, SiblingOverlap: 1},
			forcedOutcome: remediationOutcomeDidNotConverge,
			wantCauses:    []remediationCause{remediationCauseConflict, remediationCauseSiblingOverlap},
			wantUnparks:   true,
		},
		{
			// #3941: remediation was working a substantive finding when the
			// reviewer rejected it. That is the PR's own content, and the
			// cumulative attempt set is what keeps the whole park.
			name:          "a substantive attempt keeps the park despite a curable sibling cause",
			priorAttempts: remediationAttempts{Substantive: 1, SiblingOverlap: 1},
			forcedOutcome: remediationOutcomeDidNotConverge,
			wantCauses:    []remediationCause{remediationCauseSubstantive, remediationCauseSiblingOverlap},
		},
		{
			name:          "a forced escalation with nothing attempted still holds",
			forcedOutcome: remediationOutcomeDidNotConverge,
		},
		{
			name:          "failing CI attempted under a forced escalation is curable (#4058)",
			priorAttempts: remediationAttempts{FailingCI: 2},
			forcedOutcome: remediationOutcomeDidNotConverge,
			wantCauses:    []remediationCause{remediationCauseFailingCI},
			wantUnparks:   true,
		},
		{
			name:          "an infrastructure outcome is refused on the outcome, whatever was attempted",
			priorAttempts: remediationAttempts{Conflict: 1},
			forcedOutcome: remediationOutcomeInfrastructure,
			wantCauses:    []remediationCause{remediationCauseConflict},
		},
		{
			name:           "a policy exclusion records nothing and holds",
			priorAttempts:  remediationAttempts{Conflict: 1},
			forcedOutcome:  remediationOutcomeDidNotConverge,
			policyExcluded: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
				Prior:          remediationState{Cycles: 1, AttemptsByCause: tt.priorAttempts},
				Forced:         true,
				ForcedReason:   "the in-run reviewer returned a terminal `fail` verdict on this remediation attempt",
				ForcedOutcome:  tt.forcedOutcome,
				PolicyExcluded: tt.policyExcluded,
				HeadSHA:        "h1",
				BaseSHA:        "base-at-escalation",
			})
			if !decision.Escalated {
				t.Fatal("escalated = false, want true — a forced escalation always escalates")
			}
			if !reflect.DeepEqual(decision.State.EscalationCauses, tt.wantCauses) {
				t.Fatalf("escalation causes = %v, want %v", decision.State.EscalationCauses, tt.wantCauses)
			}
			if got := escalationBaseAdvanceUnparks(decision.State); got != tt.wantUnparks {
				t.Fatalf("base advance unparks = %t, want %t", got, tt.wantUnparks)
			}
		})
	}
}

// TestForcedEscalationCauseIsNotThisCyclesCause guards the source of the
// attribution. in.Causes is what THIS cycle observed, which on the forced path
// is whatever the caller happened to pass while no agent ran; the park has to
// be attributed to what remediation actually attempted across its cycles, which
// is the cumulative set the same payload already publishes as attemptedCauses.
func TestForcedEscalationCauseIsNotThisCyclesCause(t *testing.T) {
	decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior:         remediationState{Cycles: 2, AttemptsByCause: remediationAttempts{Substantive: 1}},
		Causes:        []remediationCause{remediationCauseConflict},
		Forced:        true,
		ForcedReason:  "the in-run reviewer returned a terminal `fail` verdict on this remediation attempt",
		ForcedOutcome: remediationOutcomeDidNotConverge,
		HeadSHA:       "h1",
		BaseSHA:       "base-at-escalation",
	})
	want := []remediationCause{remediationCauseSubstantive}
	if !reflect.DeepEqual(decision.State.EscalationCauses, want) {
		t.Fatalf("escalation causes = %v, want %v — the attempted set, not this cycle's input",
			decision.State.EscalationCauses, want)
	}
	if escalationBaseAdvanceUnparks(decision.State) {
		t.Fatal("base advance unparks a PR whose remediation was working a substantive finding")
	}
	if !reflect.DeepEqual(decision.State.EscalationCauses, decision.State.AttemptedCauses) {
		t.Fatalf("escalation causes %v and attempted causes %v disagree — the reader must see what the writer recorded",
			decision.State.EscalationCauses, decision.State.AttemptedCauses)
	}
}
