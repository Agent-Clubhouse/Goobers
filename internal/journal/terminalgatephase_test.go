package journal

import "testing"

// TestPhaseFromEventsTreatsTerminalGatesAsTerminal verifies that a gate
// resolving to a reserved terminal
// target ends the run, and the gate.evaluated event is the terminal record.
// Treating only run.finished as terminal reported such runs PhaseRunning
// forever and prevent claim recovery.
//
// The "@join" and named-target cases are the discriminating half: a branch
// target and an ordinary gate transition must NOT terminalize a live run, or
// every parallel workflow would report terminal mid-flight and release claims
// it still needs.
func TestPhaseFromEventsTreatsTerminalGatesAsTerminal(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		want   RunPhase
	}{
		{
			name: "abort gate terminalizes without run.finished",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Verdict: "fail", Target: TargetAbort},
			},
			want: PhaseAborted,
		},
		{
			name: "escalate gate terminalizes without run.finished",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: TargetEscalate},
			},
			want: PhaseEscalated,
		},
		{
			name: "join target ends a branch, not the run",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "branch-gate", Verdict: "pass", Target: TargetJoin},
			},
			want: PhaseRunning,
		},
		{
			name: "ordinary named gate target keeps the run running",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "implement"},
			},
			want: PhaseRunning,
		},
		{
			name: "gate with an empty target keeps the run running",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "review", Verdict: "pass"},
			},
			want: PhaseRunning,
		},
		{
			name: "a stage after a join gate still reads running",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "branch-gate", Target: TargetJoin},
				{Type: EventStageStarted, Stage: "merge-pr"},
			},
			want: PhaseRunning,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseFromEvents(tc.events); got != tc.want {
				t.Fatalf("phase = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPhaseFromEventsDistinguishesPendingHumanDecision pins the boundary
// between the two ways a terminal gate.evaluated reaches the log.
//
// The runner EXECUTING a gate emits gate.paused -> gate.started ->
// gate.evaluated, and that is terminal even if run.finished is not recorded.
// A HUMAN decision is
// recorded out-of-band by gate.Evaluator.EvaluateHuman straight onto a paused
// run, producing gate.paused -> gate.evaluated with NO gate.started: the run
// has not executed the decision yet, so it must still read running or Resume
// would refuse to replay the very decision it was handed.
func TestPhaseFromEventsDistinguishesPendingHumanDecision(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		want   RunPhase
	}{
		{
			name: "pending human rejection is not yet terminal",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGatePaused, Gate: "approval"},
				{Type: EventGateEvaluated, Gate: "approval", Verdict: "reject", Target: TargetAbort},
			},
			want: PhaseRunning,
		},
		{
			name: "pending human escalation is not yet terminal",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGatePaused, Gate: "approval"},
				{Type: EventGateEvaluated, Gate: "approval", Target: TargetEscalate},
			},
			want: PhaseRunning,
		},
		{
			name: "an interleaved sibling branch does not execute a pending human gate",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGatePaused, Gate: "approval", Branch: 1},
				{Type: EventStageFinished, Stage: "sibling-work", Branch: 2},
				{Type: EventGateEvaluated, Gate: "approval", Verdict: "reject", Target: TargetAbort, Branch: 1},
			},
			want: PhaseRunning,
		},
		{
			name: "an executed gate after the pause is terminal",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGatePaused, Gate: "merged-gate"},
				{Type: EventGateStarted, Gate: "merged-gate"},
				{Type: EventGateEvaluated, Gate: "merged-gate", Verdict: "fail", Target: TargetAbort},
			},
			want: PhaseAborted,
		},
		{
			// The abort executed, then terminal preparation failed before
			// run.finished could be written.
			name: "executed abort whose terminal preparer failed is still terminal",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventStageFinished, Stage: "merge-pr"},
				{Type: EventGatePaused, Gate: "merged-gate"},
				{Type: EventGateStarted, Gate: "merged-gate"},
				{Type: EventGateEvaluated, Gate: "merged-gate", Verdict: "fail", Target: TargetAbort},
				{Type: EventRefTouched, Error: &ErrorDetail{Code: "run_abort_label_failed", Message: "401"}},
			},
			want: PhaseAborted,
		},
		{
			name: "a bare terminal gate with no gate records is treated as executed",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
			},
			want: PhaseAborted,
		},
		{
			name: "a pause on a DIFFERENT gate does not make this one pending",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGatePaused, Gate: "approval"},
				{Type: EventGateStarted, Gate: "approval"},
				{Type: EventGateEvaluated, Gate: "approval", Target: "implement"},
				{Type: EventStageFinished, Stage: "implement"},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
			},
			want: PhaseAborted,
		},
		{
			// Once the runner resumes and executes the decision, the run really
			// does end.
			name: "resumed and executed human rejection terminalizes",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGatePaused, Gate: "approval"},
				{Type: EventGateEvaluated, Gate: "approval", Verdict: "reject", Target: TargetAbort},
				{Type: EventRunResumed},
				{Type: EventRunFinished, Status: string(PhaseAborted)},
			},
			want: PhaseAborted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseFromEvents(tc.events); got != tc.want {
				t.Fatalf("phase = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPhaseFromEventsTerminalGatePrecedence pins the newest-first scan's
// ordering guarantees around a terminal gate. A terminal gate is only the
// LATEST word until something later contradicts it: an explicit run.finished,
// a resume, or a re-entry all outrank a previous attempt's abort. Without this,
// a resumed run would report aborted and its live claims would be released out
// from under it — the mirror image of the original bug.
func TestPhaseFromEventsTerminalGatePrecedence(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		want   RunPhase
	}{
		{
			name: "run.finished after an abort gate wins",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
				{Type: EventRunFinished, Status: string(PhaseAborted)},
			},
			want: PhaseAborted,
		},
		{
			name: "a run.finished recording a different status still wins",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "review", Target: TargetEscalate},
				{Type: EventRunFinished, Status: string(PhaseCompleted)},
			},
			want: PhaseCompleted,
		},
		{
			name: "a resume after an abort gate makes the run running again",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
				{Type: EventRunFinished, Status: string(PhaseAborted)},
				{Type: EventRunResumed},
			},
			want: PhaseRunning,
		},
		{
			name: "a resume directly after a terminal gate re-enters the run",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
				{Type: EventRunResumed},
			},
			want: PhaseRunning,
		},
		{
			name: "a resumed run that aborts again reads aborted",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
				{Type: EventRunResumed},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
			},
			want: PhaseAborted,
		},
		{
			name: "a later join gate does not undo an earlier abort",
			events: []Event{
				{Type: EventRunStarted},
				{Type: EventGateEvaluated, Gate: "merged-gate", Target: TargetAbort},
				{Type: EventGateEvaluated, Gate: "branch-gate", Target: TargetJoin},
			},
			want: PhaseAborted,
		},
		{
			name: "no events at all is running",
			want: PhaseRunning,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseFromEvents(tc.events); got != tc.want {
				t.Fatalf("phase = %q, want %q", got, tc.want)
			}
		})
	}
}
