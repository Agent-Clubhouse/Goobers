package readmodel

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestGateWaitProjectionMatchesJournalAcrossRestartAndTelemetry(t *testing.T) {
	sequence := []journal.EventType{
		journal.EventRunStarted, journal.EventGatePaused,
		journal.EventRunnerAnnotation, journal.EventStageHeartbeat,
		journal.EventGateStarted, journal.EventGateEvaluated,
		journal.EventGatePaused, journal.EventRunResumed,
		journal.EventGatePaused, journal.EventRunFinished,
	}
	var state StageActivity
	var events []journal.Event
	for _, kind := range sequence {
		event := journal.Event{Schema: journal.EventSchema, Type: kind, Gate: "approval"}
		events = append(events, event)
		state = state.After(event)
		if state.WaitingForGate != journal.ParkedAtGate(events) {
			t.Fatalf("%s: projected wait=%v canonical wait=%v", kind, state.WaitingForGate, journal.ParkedAtGate(events))
		}
		// Operator facts survive a projector/process restart in this JSON form.
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var resumed StageActivity
		if err := json.Unmarshal(data, &resumed); err != nil {
			t.Fatal(err)
		}
		state = resumed
	}
}
