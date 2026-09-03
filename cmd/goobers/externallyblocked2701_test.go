package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestExternallyBlockedRemediationCauses pins the classification predicate:
// only a cause no in-worktree change can resolve, and only under its own
// evidence, counts as external (#2701).
func TestExternallyBlockedRemediationCauses(t *testing.T) {
	tests := []struct {
		name          string
		labels        []string
		causes        []remediationCause
		baseCIFailing bool
		want          bool
	}{
		{
			name:          "failing CI red on the base branch is not this diff's doing",
			causes:        []remediationCause{remediationCauseFailingCI},
			baseCIFailing: true,
			want:          true,
		},
		{
			name:   "failing CI on a green base is this diff's own problem",
			causes: []remediationCause{remediationCauseFailingCI},
		},
		{
			name:   "recorded sibling sequencing is external",
			labels: []string{blockedOnSiblingLabel},
			causes: []remediationCause{remediationCauseSiblingOverlap},
			want:   true,
		},
		{
			name:   "an unrecorded overlap finding is ordinary in-worktree work",
			causes: []remediationCause{remediationCauseSiblingOverlap},
		},
		{
			name:          "a mixed but wholly external set still parks",
			labels:        []string{blockedOnSiblingLabel},
			causes:        []remediationCause{remediationCauseSiblingOverlap, remediationCauseFailingCI},
			baseCIFailing: true,
			want:          true,
		},
		{
			name:          "one actionable cause makes the whole cycle actionable",
			labels:        []string{blockedOnSiblingLabel},
			causes:        []remediationCause{remediationCauseSiblingOverlap, remediationCauseSubstantive},
			baseCIFailing: true,
		},
		{
			name:          "no detected cause is not an external block",
			labels:        []string{blockedOnSiblingLabel},
			baseCIFailing: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := externallyBlockedRemediationCauses(tt.labels, tt.causes, tt.baseCIFailing); got != tt.want {
				t.Fatalf("externallyBlockedRemediationCauses = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestExternallyBlockedParkLeavesABaseAdvanceExit proves the park records the
// external cause set, so escalationStillBlocks releases it the moment the base
// advances — the sibling landed, or CI on the base recovered — rather than
// needing a human.
func TestExternallyBlockedParkLeavesABaseAdvanceExit(t *testing.T) {
	decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Causes:              []remediationCause{remediationCauseFailingCI},
		Budgets:             remediationBudgets{FailingCI: 2},
		Digest:              "sha256:current",
		HeadSHA:             "head-sha",
		BaseSHA:             "base-sha",
		ExternallyBlocked:   true,
		ExternalBlockReason: "required CI is red on the base branch itself",
	})
	if !decision.Escalated {
		t.Fatal("an externally-blocked cycle must park instead of running the agentic chain")
	}
	if decision.Escalation.Outcome != remediationOutcomeExternallyBlocked {
		t.Fatalf("outcome = %q, want %q", decision.Escalation.Outcome, remediationOutcomeExternallyBlocked)
	}
	if decision.Escalation.Attempted {
		t.Fatal("no agent ran, so the park must not claim remediation was attempted")
	}
	if got := decision.State.AttemptsByCause.forCause(remediationCauseFailingCI); got != 0 {
		t.Fatalf("failing-ci attempts = %d, want 0 — an external park consumes no allowance", got)
	}
	if !escalationBaseAdvanceUnparks(decision.State) {
		t.Fatalf("park %+v has no base-advance exit", decision.State)
	}
}

// TestExternalBlockYieldsToAConcreteFinding proves the classification never
// masks a more specific account of why the PR is stuck.
func TestExternalBlockYieldsToAConcreteFinding(t *testing.T) {
	decision := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior:               remediationState{AttemptsByCause: remediationAttempts{FailingCI: 2}},
		Causes:              []remediationCause{remediationCauseFailingCI},
		Budgets:             remediationBudgets{FailingCI: 2},
		Digest:              "sha256:current",
		ExternallyBlocked:   true,
		ExternalBlockReason: "required CI is red on the base branch itself",
	})
	if decision.Escalation.Outcome != remediationOutcomeBudgetExhausted {
		t.Fatalf("outcome = %q, want %q", decision.Escalation.Outcome, remediationOutcomeBudgetExhausted)
	}
}

// TestRemediationCheckpointParksBaseRedCIBeforeTheAgenticChain is the #2701
// end-to-end: a PR whose only cause is failing CI that is already red on its
// base parks at the checkpoint, so pr-remediation never spends implement on a
// repass that can only reproduce the identical diff.
func TestRemediationCheckpointParksBaseRedCIBeforeTheAgenticChain(t *testing.T) {
	tests := []struct {
		name           string
		baseCheckState string
		wantParked     bool
	}{
		{name: "base CI red parks the PR", baseCheckState: "failure", wantParked: true},
		{name: "base CI green remediates as before", baseCheckState: "success"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
			st := &remediationCheckpointServerState{
				number: 77, headSHA: headSHA, baseSHA: baseSHA,
				liveBaseSHA:    "0000000000000000000000000000000000002701",
				baseCheckState: tt.baseCheckState,
				labels:         []string{needsRemediationLabel},
			}
			server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
			instanceRoot := remediationCheckpointEnv(t, server.URL, false)
			resultFile := filepath.Join(t.TempDir(), "checkpoint-result.json")
			t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
			t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", string(remediationCauseFailingCI))

			code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}

			result := readCheckpointResult(t, resultFile)
			if tt.wantParked {
				if result["continueRemediation"] != "false" {
					t.Fatalf("continueRemediation = %q, want false — the agentic chain must not run", result["continueRemediation"])
				}
				if result["escalationOutcome"] != string(remediationOutcomeExternallyBlocked) {
					t.Fatalf("escalationOutcome = %q, want %q", result["escalationOutcome"], remediationOutcomeExternallyBlocked)
				}
				if result["remediationAttempted"] != "false" {
					t.Fatalf("remediationAttempted = %q, want false", result["remediationAttempted"])
				}
			} else {
				if result["continueRemediation"] != "true" {
					t.Fatalf("continueRemediation = %q, want true — a red PR on a green base is still the agent's work", result["continueRemediation"])
				}
				if result["escalationOutcome"] != "" {
					t.Fatalf("escalationOutcome = %q, want empty", result["escalationOutcome"])
				}
			}

			st.mu.Lock()
			defer st.mu.Unlock()
			if len(st.comments) != 1 {
				t.Fatalf("comments = %v, want exactly one sticky checkpoint comment", st.comments)
			}
			state, ok := parseRemediationStateComment(st.comments[0])
			if !ok {
				t.Fatalf("sticky comment %q carries no state payload", st.comments[0])
			}
			if state.Escalated != tt.wantParked {
				t.Fatalf("escalated = %t, want %t (comment = %q)", state.Escalated, tt.wantParked, st.comments[0])
			}
			if got := state.AttemptsByCause.forCause(remediationCauseFailingCI); tt.wantParked && got != 0 {
				t.Fatalf("failing-ci attempts = %d, want 0 — an external park consumes no allowance", got)
			}
			if !tt.wantParked {
				return
			}
			if !strings.Contains(state.EscalatedReason, "external to this PR's own diff") {
				t.Fatalf("escalation reason = %q, want the external-block account", state.EscalatedReason)
			}
			if !hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
				t.Fatalf("labels = %v, want the park recorded as %s", st.labels, remediationEscalatedLabel)
			}
			if state.EscalatedBaseSHA != st.liveBaseSHA {
				t.Fatalf("escalatedBaseSha = %q, want the live base tip %q so a base advance releases the park",
					state.EscalatedBaseSHA, st.liveBaseSHA)
			}
		})
	}
}
