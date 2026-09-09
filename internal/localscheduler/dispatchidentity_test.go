package localscheduler

import (
	"strings"
	"testing"
	"time"
)

func TestTriggerDispatchPreservesAssignedRunIdentity(t *testing.T) {
	const assigned = "1234567890abcdef1234567890abcdef"
	for _, mode := range []string{"unqualified", "exact", "priority"} {
		t.Run(mode, func(t *testing.T) {
			starter := &fakeStarter{}
			s := New([]WorkflowEntry{dispatchContextEntry(starter)}, nil)
			identity := WorkflowIdentity{Gaggle: "web", Workflow: "implement"}
			var id string
			var err error
			switch mode {
			case "unqualified":
				id, err = s.TriggerWithDispatchContextOptions(t.Context(), t.Context(), "implement", time.Now(), ManualTriggerOptions{RunID: assigned})
			case "exact":
				id, err = s.TriggerExactWithDispatchContextOptions(t.Context(), t.Context(), identity, time.Now(), ManualTriggerOptions{RunID: assigned})
			case "priority":
				id, err = s.TriggerPriorityWithDispatchRunID(t.Context(), t.Context(), identity, "source-run", time.Now(), assigned)
			}
			if err != nil || id != assigned {
				t.Fatalf("dispatch = %q, %v", id, err)
			}
			s.Wait()
			if starter.count() != 1 || starter.starts[0].RunID != assigned {
				t.Fatalf("starter requests = %+v", starter.starts)
			}
		})
	}
}

func TestTriggerDispatchRejectsInvalidAssignedIdentityBeforeStarting(t *testing.T) {
	for _, assigned := range []string{"../escape", strings.Repeat("0", 32), strings.Repeat("a", 31), strings.Repeat("A", 32)} {
		t.Run(assigned, func(t *testing.T) {
			starter := &fakeStarter{}
			s := New([]WorkflowEntry{dispatchContextEntry(starter)}, nil)
			id, err := s.TriggerWithOptions(t.Context(), "implement", time.Now(), ManualTriggerOptions{RunID: assigned})
			if err == nil || id != "" {
				t.Fatalf("invalid ID dispatched: %q, %v", id, err)
			}
			s.Wait()
			if starter.count() != 0 {
				t.Fatal("invalid ID reached starter")
			}
		})
	}
}
