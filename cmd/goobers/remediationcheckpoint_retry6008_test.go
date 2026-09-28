package main

import (
	"reflect"
	"strings"
	"testing"
)

// #6008: remediation-checkpoint records its state (with the diff digest)
// before the agentic chain runs. A retry of the same stage in the same attempt
// read that write back as the previous cycle and parked the PR as a
// no-progress stall. These tests pin that a same-attempt retry re-derives the
// cycle it already recorded, while a byte-identical diff across attempts
// still escalates.

func retry6008Input(prior remediationState, runID string) remediationCheckpointDecisionInput {
	return remediationCheckpointDecisionInput{
		Prior:     prior,
		Causes:    []remediationCause{remediationCauseSubstantive},
		Budgets:   remediationBudgets{Substantive: 2},
		Digest:    "sha256:same",
		HeadSHA:   "head",
		BaseSHA:   "base",
		Watermark: "2026-09-01T00:00:00Z",
		RunID:     runID,
	}
}

func TestDecideRemediationCheckpointSameAttemptRetry(t *testing.T) {
	// The attempt's own first write: cycle 2 of a PR whose previous attempt
	// (another run, another head) recorded a different digest.
	previous := remediationState{
		Cycles: 1, AttemptsByCause: remediationAttempts{Substantive: 1},
		LastDiffDigest: "sha256:previous", HeadSHA: "old-head", BaseSHA: "base",
		RunID: "run-a", ChargedCauses: []remediationCause{remediationCauseSubstantive},
	}
	first := decideRemediationCheckpoint(retry6008Input(previous, "run-b"))
	if first.Escalated || first.State.Cycles != 2 || first.State.AttemptsByCause.Substantive != 2 {
		t.Fatalf("first write = %+v, want advancing cycle 2 with substantive 2/2", first)
	}

	t.Run("a retry of the same attempt re-derives its own cycle", func(t *testing.T) {
		retry := decideRemediationCheckpoint(retry6008Input(first.State, "run-b"))
		if retry.Escalated {
			t.Fatalf("retry escalated (%s: %s), want the same advancing cycle", retry.Escalation.Outcome, retry.Escalation.Reason)
		}
		if !reflect.DeepEqual(retry, first) {
			t.Fatalf("retry decision = %+v\nwant the first write's decision = %+v", retry, first)
		}
	})

	t.Run("infrastructure failures the first write carried are kept", func(t *testing.T) {
		prior := first.State
		prior.InfrastructureFailures = 2
		retry := decideRemediationCheckpoint(retry6008Input(prior, "run-b"))
		if retry.Escalated || retry.State.InfrastructureFailures != 2 {
			t.Fatalf("retry = %+v, want an advancing cycle carrying 2 infrastructure failures", retry)
		}
	})

	for _, tc := range []struct {
		name    string
		mutate  func(*remediationState)
		runID   string
		outcome remediationEscalationOutcome
		reason  string
	}{
		{
			name:   "a later attempt with a byte-identical diff still stalls",
			runID:  "run-c",
			reason: "byte-identical",
		},
		{
			name:   "a record without a run id reads as a previous attempt",
			mutate: func(s *remediationState) { s.RunID = "" },
			runID:  "run-b",
			reason: "byte-identical",
		},
		{
			name:   "an unknown current run cannot claim the record",
			runID:  "",
			reason: "byte-identical",
		},
		{
			name:   "the same run at a different head is not a retry",
			mutate: func(s *remediationState) { s.HeadSHA = "earlier-head" },
			runID:  "run-b",
			reason: "byte-identical",
		},
		{
			name:    "an escalation record is never re-entered",
			mutate:  func(s *remediationState) { s.Escalated = true },
			runID:   "run-b",
			outcome: remediationOutcomeBudgetExhausted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := first.State
			prior.ChargedCauses = append([]remediationCause(nil), prior.ChargedCauses...)
			if tc.mutate != nil {
				tc.mutate(&prior)
			}
			got := decideRemediationCheckpoint(retry6008Input(prior, tc.runID))
			if !got.Escalated {
				t.Fatalf("decision = %+v, want an escalation", got)
			}
			if tc.reason != "" && !strings.Contains(got.Escalation.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", got.Escalation.Reason, tc.reason)
			}
			if tc.outcome != "" && got.Escalation.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", got.Escalation.Outcome, tc.outcome)
			}
		})
	}
}

// TestRemediationCheckpointSameRunRetryDoesNotEscalate drives the stage: a
// retry under the same run at the same pushed head records the same cycle and
// leaves the labels alone; the next attempt (a new run) with the unchanged
// diff still parks the PR.
func TestRemediationCheckpointSameRunRetryDoesNotEscalate(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{needsRemediationLabel}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	for attempt := 1; attempt <= 2; attempt++ {
		code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
		if code != 0 {
			t.Fatalf("run %d: code = %d, stdout = %q, stderr = %q", attempt, code, stdout, stderr)
		}
		if strings.Contains(stdout, "byte-identical") || strings.Contains(stdout, "escalated") {
			t.Fatalf("run %d stdout = %q, want no escalation on a same-attempt retry", attempt, stdout)
		}
	}
	st.mu.Lock()
	if len(st.comments) != 1 {
		st.mu.Unlock()
		t.Fatalf("comments = %q, want the one sticky state comment", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	labels := append([]string(nil), st.labels...)
	st.mu.Unlock()
	if !ok || state.Escalated || state.Cycles != 1 || state.AttemptsByCause.Substantive != 1 || state.RunID != "run-364" {
		t.Fatalf("state after retry = %+v (ok=%v), want cycle 1 charged once to run-364", state, ok)
	}
	if !reflect.DeepEqual(labels, []string{needsRemediationLabel}) {
		t.Fatalf("labels after retry = %v, want only %s", labels, needsRemediationLabel)
	}

	t.Setenv("GOOBERS_RUN_ID", "run-364-next")
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("next attempt: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "byte-identical") {
		t.Fatalf("next attempt stdout = %q, want the byte-identical stall", stdout)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, want %s on a repeat across attempts", st.labels, remediationEscalatedLabel)
	}
}

// TestForcedEscalationInSameRunKeepsAttemptCharge pins that a forced
// (--escalate) checkpoint is never read as a same-attempt retry. The reference
// workflow's park stages invoke it in the same run, at the same head, right
// after this attempt's own checkpoint; unwinding that record would refund the
// attempt's charge and lose the #4074 cause attribution (a rejected first-cycle
// rebase would stay parked with no base-advance exit).
func TestForcedEscalationInSameRunKeepsAttemptCharge(t *testing.T) {
	advance := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Causes:  []remediationCause{remediationCauseConflict},
		Budgets: remediationBudgets{Conflict: 2},
		Digest:  "sha256:rebase",
		HeadSHA: "head",
		BaseSHA: "base",
		RunID:   "run-x",
	})
	if advance.Escalated || advance.State.AttemptsByCause.Conflict != 1 {
		t.Fatalf("advance = %+v, want an advancing cycle charging conflict once", advance)
	}

	forced := decideRemediationCheckpoint(remediationCheckpointDecisionInput{
		Prior:         advance.State,
		Causes:        []remediationCause{remediationCauseConflict},
		Budgets:       remediationBudgets{Conflict: 2},
		Forced:        true,
		ForcedReason:  "the in-run reviewer returned a terminal `fail` verdict on this remediation attempt",
		ForcedOutcome: remediationOutcomeDidNotConverge,
		Digest:        "sha256:rebase",
		HeadSHA:       "head",
		BaseSHA:       "base",
		RunID:         "run-x",
	})
	if !forced.Escalated {
		t.Fatal("escalated = false, want true — a forced escalation always escalates")
	}
	want := []remediationCause{remediationCauseConflict}
	if forced.State.AttemptsByCause.Conflict != 1 {
		t.Fatalf("attempts = %+v, want the attempt's conflict charge kept", forced.State.AttemptsByCause)
	}
	if !reflect.DeepEqual(forced.State.AttemptedCauses, want) {
		t.Fatalf("attempted causes = %v, want %v", forced.State.AttemptedCauses, want)
	}
	if !reflect.DeepEqual(forced.State.EscalationCauses, want) {
		t.Fatalf("escalation causes = %v, want %v", forced.State.EscalationCauses, want)
	}
	if !escalationBaseAdvanceUnparks(forced.State) {
		t.Fatal("base advance unparks = false, want true for a rejected rebase")
	}
}

// TestRemediationCheckpointEscalateInSameRunKeepsAttemptCharge drives the
// stage the way the reference workflow does: remediation-checkpoint, then a
// park stage's remediation-checkpoint --escalate under the same GOOBERS_RUN_ID
// at an unchanged head.
func TestRemediationCheckpointEscalateInSameRunKeepsAttemptCharge(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{needsRemediationLabel}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "conflict")

	if code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot); code != 0 {
		t.Fatalf("checkpoint: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--escalate", "reviewer rejected", instanceRoot); code != 0 {
		t.Fatalf("escalate: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	var state remediationState
	found := false
	for _, body := range st.comments {
		if parsed, ok := parseRemediationStateComment(body); ok {
			state, found = parsed, true
		}
	}
	if !found || !state.Escalated {
		t.Fatalf("state = %+v (found=%v), want an escalated state record", state, found)
	}
	want := []remediationCause{remediationCauseConflict}
	if state.AttemptsByCause.Conflict != 1 || !reflect.DeepEqual(state.EscalationCauses, want) {
		t.Fatalf("state = %+v, want the attempt's conflict charge kept and attributed", state)
	}
	if !escalationBaseAdvanceUnparks(state) {
		t.Fatal("base advance unparks = false, want true for a rejected rebase")
	}
}
