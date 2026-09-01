package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// A park recorded before #4074 carries attemptedCauses but no escalationCauses,
// and a parked PR is excluded from remediation, so nothing ever rewrites the
// record to add them. Without a read-side fallback the #4074 exit reaches only
// PRs parked after the upgrade — never the ones that motivated it.
//
// The payloads below are the live ones read off Agent-Clubhouse/Goobers on
// 2026-08-31, trimmed to the fields this decision consults.
func TestLegacyForcedParkFallsBackToAttemptedCauses(t *testing.T) {
	const forcedReason = "the in-run reviewer returned a terminal `fail` verdict on this remediation attempt — the approach itself was rejected, not just its details (design doc §4 D2)"

	for _, tc := range []struct {
		name  string
		state remediationState
		want  bool
	}{
		{
			name: "live #3894 attempted a conflict rebase only",
			state: remediationState{
				Escalated: true, EscalatedReason: forcedReason,
				EscalationOutcome:    remediationOutcomeDidNotConverge,
				RemediationAttempted: true,
				AttemptedCauses:      []remediationCause{remediationCauseConflict},
				EscalationGeneration: 1,
			},
			want: true,
		},
		{
			name: "live #3900 attempted conflict and sibling-overlap",
			state: remediationState{
				Escalated: true, EscalatedReason: forcedReason,
				EscalationOutcome:    remediationOutcomeDidNotConverge,
				RemediationAttempted: true,
				AttemptedCauses:      []remediationCause{remediationCauseConflict, remediationCauseSiblingOverlap},
				EscalationGeneration: 1,
			},
			want: true,
		},
		{
			name: "live #3941 attempted a substantive fix, which no rebase cures",
			state: remediationState{
				Escalated: true, EscalatedReason: forcedReason,
				EscalationOutcome:    remediationOutcomeDidNotConverge,
				RemediationAttempted: true,
				AttemptedCauses:      []remediationCause{remediationCauseSubstantive, remediationCauseSiblingOverlap},
				EscalationGeneration: 1,
			},
			want: false,
		},
		{
			name: "an explicit cause set still wins over the fallback",
			state: remediationState{
				Escalated:            true,
				EscalationOutcome:    remediationOutcomeDidNotConverge,
				AttemptedCauses:      []remediationCause{remediationCauseConflict},
				EscalationCauses:     []remediationCause{remediationCauseSubstantive},
				EscalationGeneration: 1,
			},
			want: false,
		},
		{
			name: "a deliberately cleared cause set is not resurrected",
			state: remediationState{
				Escalated:               true,
				EscalationOutcome:       remediationOutcomeDidNotConverge,
				AttemptedCauses:         []remediationCause{remediationCauseConflict},
				EscalationCausesCleared: true,
				EscalationGeneration:    1,
			},
			want: false,
		},
		{
			name: "a later generation is not eligible for the fallback",
			state: remediationState{
				Escalated:            true,
				EscalationOutcome:    remediationOutcomeDidNotConverge,
				AttemptedCauses:      []remediationCause{remediationCauseConflict},
				EscalationGeneration: 2,
			},
			want: false,
		},
		{
			name: "a policy exclusion is refused before the fallback is consulted",
			state: remediationState{
				Escalated:            true,
				EscalationOutcome:    remediationOutcomePolicyExcluded,
				AttemptedCauses:      []remediationCause{remediationCauseConflict},
				EscalationGeneration: 1,
			},
			want: false,
		},
		{
			name: "a structural collision is refused before the fallback is consulted",
			state: remediationState{
				Escalated:                  true,
				EscalationOutcome:          remediationOutcomeDidNotConverge,
				AttemptedCauses:            []remediationCause{remediationCauseConflict},
				StructuralCollisionContext: "helpers.go rewritten by #3904",
				EscalationGeneration:       1,
			},
			want: false,
		},
		{
			name: "no cause anywhere still holds",
			state: remediationState{
				Escalated:            true,
				EscalationOutcome:    remediationOutcomeDidNotConverge,
				EscalationGeneration: 1,
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escalationBaseAdvanceUnparks(tc.state); got != tc.want {
				t.Fatalf("escalationBaseAdvanceUnparks = %v, want %v", got, tc.want)
			}
		})
	}
}

// The marker exists so the fallback can tell "never populated" from "emptied
// on purpose". A repeat fail at a NEW head resets the generation to 1, which is
// exactly the shape the fallback targets, so without the marker the refresh
// would hand back the very cause it just dropped.
func TestRepeatFailRefreshAtANewHeadMarksTheCauseSetCleared(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	comment, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedReason:      "conflict budget exhausted",
		EscalatedHeadSHA:     "h1",
		EscalatedBaseSHA:     "base-at-escalation",
		AttemptedCauses:      []remediationCause{remediationCauseConflict},
		EscalationCauses:     []remediationCause{remediationCauseConflict},
		EscalationGeneration: 1,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(1, "re-failed pr")
	server.addComment(1, comment)
	server.setBranchTip("main", "base-after-sibling-merge")
	provider := server.newGitHubProvider("token")
	pr := providers.PullRequestSummary{
		Number: 1, Base: "main", HeadSHA: "h2", BaseSHA: "base-at-escalation",
		Labels: []string{remediationEscalatedLabel},
	}

	comments, err := provider.ListComments(context.Background(), repo, "1")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if err := refreshEscalationSnapshotAfterRepeatFail(context.Background(), provider, repo, pr, comments); err != nil {
		t.Fatalf("refreshEscalationSnapshotAfterRepeatFail: %v", err)
	}

	after, err := provider.ListComments(context.Background(), repo, "1")
	if err != nil {
		t.Fatalf("ListComments after refresh: %v", err)
	}
	state, _, found := latestRemediationState(after)
	if !found {
		t.Fatalf("comments = %v, want the sticky state edited in place", after)
	}
	if state.EscalationGeneration != 1 {
		t.Fatalf("escalation generation = %d, want 1 (a new head)", state.EscalationGeneration)
	}
	if len(state.EscalationCauses) != 0 {
		t.Fatalf("escalation causes = %v, want none after a reviewer re-fail", state.EscalationCauses)
	}
	if !state.EscalationCausesCleared {
		t.Fatal("EscalationCausesCleared = false, want true: a deliberate clear must be distinguishable from one never written")
	}
	if escalationBaseAdvanceUnparks(state) {
		t.Fatal("a base advance released a park a reviewer re-failed, resurrecting the dropped cause")
	}
}
