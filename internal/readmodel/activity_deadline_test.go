package readmodel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestExecutionDeadlineMatchesCurrentAttemptAndClears(t *testing.T) {
	now := time.Now().UTC()
	start := journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "work", Branch: 2, Attempt: 1, Time: now}
	state := (StageActivity{}).After(start)
	event := journal.Event{Schema: journal.EventSchema, Type: journal.EventRunnerAnnotation, Stage: "work", Branch: 2, Attempt: 1, Time: now.Add(time.Second), Runner: map[string]any{"kind": "execution-deadline.v1", "executionId": "one", "deadline": now.Add(time.Hour).Format(time.RFC3339Nano), "executionState": "active"}}
	prior := state
	state = state.After(event)
	if state.Active[0].ExecutionDeadline == nil || prior.Active[0].ExecutionDeadline != nil {
		t.Fatal("deadline missing or snapshot mutated")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored StageActivity
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Active[0].ExecutionDeadline == nil {
		t.Fatal("projection serialization lost evidence")
	}
	event.Runner["executionState"] = "finished"
	restored = restored.After(event)
	if restored.Active[0].ExecutionDeadline != nil {
		t.Fatal("process return retained deadline")
	}
	event.Runner["executionState"], event.Runner["executionId"] = "active", "two"
	restored = restored.After(event)
	if restored.Active[0].ExecutionDeadline == nil {
		t.Fatal("serial invocation did not establish own bound")
	}
	start.Attempt, start.Time = 2, now.Add(2*time.Second)
	restored = restored.After(start).After(event)
	if restored.Active[0].ExecutionDeadline != nil {
		t.Fatal("old attempt bound applied to retry")
	}
	event.Attempt = 2
	restored = restored.After(event)
	if restored.Active[0].ExecutionDeadline != nil {
		t.Fatal("pre-start bound applied to new traversal")
	}
	event.Time = now.Add(3 * time.Second)
	restored = restored.After(event)
	if restored.Active[0].ExecutionDeadline == nil {
		t.Fatal("new attempt deadline not accepted")
	}
	restored = restored.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventRunResumed})
	if len(restored.Active) != 0 {
		t.Fatal("resume retained execution evidence")
	}
}

func TestOverlappingProcessDeadlinesStayUnknown(t *testing.T) {
	now := time.Now().UTC()
	state := (StageActivity{}).After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "work", Attempt: 1, Time: now})
	event := journal.Event{Schema: journal.EventSchema, Type: journal.EventRunnerAnnotation, Stage: "work", Attempt: 1, Time: now, Runner: map[string]any{"kind": "execution-deadline.v1", "executionId": "one", "deadline": now.Add(time.Hour).Format(time.RFC3339Nano), "executionState": "active"}}
	state = state.After(event)
	event.Runner["executionId"] = "two"
	state = state.After(event)
	if !state.Active[0].ExecutionOverlap || state.Active[0].ExecutionDeadline != nil {
		t.Fatal("one process falsely bounded overlapping stage")
	}
	event.Runner["executionState"] = "finished"
	state = state.After(event)
	if state.Active[0].ExecutionDeadline != nil {
		t.Fatal("ambiguous overlap recovered a deadline")
	}
}
