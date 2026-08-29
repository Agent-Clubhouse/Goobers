package journal

import "testing"

// TestParkedAtGate pins the predicate both stalled-run sweeps use as their one
// exemption from escalation. The cases that matter are the ones where an event
// lands AFTER the pause: the sweeps used to test only the last event, and a
// mode-3 pod emitting through the journal plane into the run's own journal
// (livejournal.Writer.Adopt) makes that shape ordinary rather than exotic.
func TestParkedAtGate(t *testing.T) {
	for name, tc := range map[string]struct {
		events []Event
		want   bool
	}{
		"empty": {events: nil, want: false},
		"parked": {events: []Event{
			{Type: EventStageFinished, Stage: "implement"},
			{Type: EventGatePaused, Gate: "approval"},
		}, want: true},

		// The regression: a pod-plane emit after the pause. Each of these once
		// read "not parked" and escalated a run still waiting for a human.
		"parked, then a pod artifact": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventArtifactRecorded, Name: "pr.json"},
		}, want: true},
		"parked, then a late agent.lifecycle (#3774's lineage)": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventAgentLifecycle, Stage: "implement"},
		}, want: true},
		"parked, then a heartbeat and a span": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventStageHeartbeat, Stage: "implement"},
			{Type: EventSpanRecorded, Name: "tool.call"},
		}, want: true},
		"parked, then an error event": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventError, Error: &ErrorDetail{Code: "emit_failed"}},
		}, want: true},

		// Control flow that genuinely moved off the gate. These must stay
		// escalatable exactly as before.
		"decided": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventGateEvaluated, Gate: "approval", Verdict: "pass"},
		}, want: false},
		"picked up by the runner": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventGateStarted, Gate: "approval"},
		}, want: false},
		"overridden": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventGateOverridden, Gate: "approval", Target: "implement"},
		}, want: false},
		"resumed": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventRunResumed, Target: "implement"},
		}, want: false},
		"moved on to a stage": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventGateEvaluated, Gate: "approval", Verdict: "pass"},
			{Type: EventStageStarted, Stage: "open-pr"},
			{Type: EventStageHeartbeat, Stage: "open-pr"},
		}, want: false},
		"finished": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventRunFinished, Status: string(PhaseCompleted)},
		}, want: false},
		"never gated": {events: []Event{
			{Type: EventRunStarted},
			{Type: EventStageStarted, Stage: "implement"},
			{Type: EventStageHeartbeat, Stage: "implement"},
		}, want: false},
		// A gate resolved earlier in the run must not park a run that has since
		// paused nowhere: the scan stops at the FIRST control-flow event.
		"an older pause, resolved": {events: []Event{
			{Type: EventGatePaused, Gate: "approval"},
			{Type: EventGateEvaluated, Gate: "approval", Verdict: "pass"},
			{Type: EventStageStarted, Stage: "open-pr"},
			{Type: EventArtifactRecorded, Name: "pr.json"},
		}, want: false},
	} {
		if got := ParkedAtGate(tc.events); got != tc.want {
			t.Errorf("%s: ParkedAtGate = %v, want %v", name, got, tc.want)
		}
	}
}
