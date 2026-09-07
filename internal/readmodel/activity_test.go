package readmodel

import (
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestStageActivityRetriesParallelBranchesAndTerminalCleanup(t *testing.T) {
	at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	start := journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "work", Branch: 1, Attempt: 1, Time: at, Runner: map[string]any{"goober": "pinned-reviewer"}}
	state := (StageActivity{}).After(start)
	prior := state
	start.Attempt, start.Time = 2, at.Add(time.Minute)
	state = state.After(start)
	if state.Active[0].StartedAt != start.Time || prior.Active[0].StartedAt != at {
		t.Fatal("retry did not reset start or mutated the prior projection")
	}
	state = state.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageFinished, Stage: "work", Branch: 1, Attempt: 1})
	if len(state.Active) != 1 {
		t.Fatal("stale finish erased retry")
	}
	start.Branch = 2
	start.Runner = nil
	state = state.After(start)
	if len(state.Active) != 2 || state.Active[1].Goober != "" {
		t.Fatal("branch collapsed or missing owner invented")
	}
	state = state.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageFinished, Stage: "work", Branch: 1, Attempt: 2})
	if len(state.Active) != 1 || state.Active[0].Branch != 2 {
		t.Fatal("finish erased sibling")
	}
	state = state.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventGateStarted, Gate: "review", Time: at, Runner: map[string]any{"goober": "reviewer"}})
	state = state.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventGateEvaluated, Gate: "review"})
	if len(state.Active) != 1 {
		t.Fatal("gate evaluation retained active timer")
	}
	if got := state.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventRunFinished}); len(got.Active) != 0 {
		t.Fatal("terminal run retained activity")
	}
}

func TestStageActivityBoundIsExplicitAndClearedOnResume(t *testing.T) {
	var state StageActivity
	for i := 0; i < MaxActiveStageTimings+5; i++ {
		state = state.After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: fmt.Sprintf("stage-%d", i), Attempt: 1})
	}
	if len(state.Active) != MaxActiveStageTimings || !state.Truncated {
		t.Fatal("activity growth is unbounded or silently truncated")
	}
	for _, kind := range []journal.EventType{journal.EventRunResumed, journal.EventGateOverridden, journal.EventRunFinished} {
		got := state.After(journal.Event{Schema: journal.EventSchema, Type: kind})
		if len(got.Active) != 0 || got.Truncated {
			t.Fatalf("%s retained previous activity", kind)
		}
	}
}
