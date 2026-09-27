package readmodel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func readinessEvent(seq uint64, category string) journal.Event {
	return ev(seq, time.Duration(seq)*time.Second, journal.EventRunnerAnnotation, func(event *journal.Event) {
		event.Stage = "implement"
		event.Runner = map[string]any{"kind": "required-mcp-readiness", "schemaVersion": 1, "adapter": "copilot-cli", "server": "goobers-io", "category": category, "connection": "ready", "inventory": "ready", "authorization": "unobservable"}
		if category == "ready" {
			event.Runner["authorization"] = "ready"
		}
	})
}

func TestRequiredMCPConditionRecoveryAndUnknownAuthorization(t *testing.T) {
	for _, category := range []string{"transport_failure", "required_tool_unavailable", "authentication_failure", "tool_authorization_failure"} {
		t.Run(category, func(t *testing.T) {
			var state *RequiredMCPState
			state = state.After(readinessEvent(1, category))
			if !state.Conditions[0].Active || state.Conditions[0].Reason != category {
				t.Fatal(state)
			}
			original := state
			state = state.After(readinessEvent(2, "check_unobservable"))
			wantActive := category == "authentication_failure" || category == "tool_authorization_failure"
			if state.Conditions[0].Active != wantActive || !original.Conditions[0].Active {
				t.Fatalf("state=%+v prior=%+v", state, original)
			}
			state = state.After(readinessEvent(3, "ready"))
			if state.Conditions[0].Active || state.Conditions[0].Reason != "" {
				t.Fatal(state)
			}
			older := state.After(readinessEvent(1, category))
			if !reflect.DeepEqual(state, older) {
				t.Fatal("old observation reopened recovered condition")
			}
		})
	}
}

func TestRequiredMCPProjectionRebuildIncrementalAndPersistence(t *testing.T) {
	events := []journal.Event{readinessEvent(1, "required_tool_unavailable"), readinessEvent(2, "check_unobservable"), readinessEvent(3, "tool_authorization_failure"), readinessEvent(4, "check_unobservable"), readinessEvent(5, "ready")}
	whole := ProjectRun(testIdentity(), Projection{}, events)
	for split := 0; split <= len(events); split++ {
		first := ProjectRun(testIdentity(), Projection{}, events[:split])
		// Operator facts use this JSON field in the durable read model.
		wire, err := json.Marshal(first.Run.Operator)
		if err != nil {
			t.Fatal(err)
		}
		var restored OperatorFacts
		if err := json.Unmarshal(wire, &restored); err != nil {
			t.Fatal(err)
		}
		first.Run.Operator = restored
		incremental := ProjectRun(testIdentity(), first, events[split:])
		if !reflect.DeepEqual(whole.Run.Operator.RequiredMCP, incremental.Run.Operator.RequiredMCP) {
			t.Fatalf("split%d whole=%+v incremental=%+v", split, whole.Run.Operator.RequiredMCP, incremental.Run.Operator.RequiredMCP)
		}
	}
}

func TestRequiredMCPProjectionScopeBoundsAndUnsupportedEvidence(t *testing.T) {
	var state *RequiredMCPState
	first := readinessEvent(1, "transport_failure")
	state = state.After(first)
	other := readinessEvent(2, "ready")
	other.Branch = 1
	state = state.After(other)
	if len(state.Conditions) != 2 || !state.Conditions[0].Active {
		t.Fatal("different branch cleared failure")
	}
	for i := 2; i <= MaxRequiredMCPConditions; i++ {
		event := readinessEvent(uint64(i+2), "ready")
		event.Stage = fmt.Sprint(i)
		state = state.After(event)
	}
	if len(state.Conditions) != MaxRequiredMCPConditions || !state.Truncated {
		t.Fatal("unbounded condition history")
	}
	unsupported := readinessEvent(1000, "ready")
	unsupported.Runner["schemaVersion"] = 2
	if state.After(unsupported) != state {
		t.Fatal("unknown schema changed condition")
	}
	unsupported.Runner["schemaVersion"] = 1
	unsupported.Runner["authorization"] = "unobservable"
	if state.After(unsupported) != state {
		t.Fatal("unverified authorization claimed ready")
	}
}

func TestRequiredMCPCrossRunRecoveryUsesSameTruthRules(t *testing.T) {
	old, _ := requiredMCPObservation(readinessEvent(1, "tool_authorization_failure"))
	old = MergeRequiredMCPCondition(RequiredMCPCondition{}, old)
	partial, _ := requiredMCPObservation(readinessEvent(2, "check_unobservable"))
	if merged := MergeRequiredMCPCondition(old, partial); !merged.Active || merged.Reason != "tool_authorization_failure" {
		t.Fatal(merged)
	}
	ready, _ := requiredMCPObservation(readinessEvent(3, "ready"))
	if merged := MergeRequiredMCPCondition(old, ready); merged.Active {
		t.Fatal(merged)
	}
}

func TestRequiredMCPCrossRunUnknownDoesNotEraseOrRelatchDenial(t *testing.T) {
	var oldRun, newRun *RequiredMCPState
	oldRun = oldRun.After(readinessEvent(1, "tool_authorization_failure"))
	newRun = newRun.After(readinessEvent(3, "ready"))
	oldRun = oldRun.After(readinessEvent(5, "check_unobservable"))
	if !oldRun.Conditions[0].Active {
		t.Fatal("unknown erased the old run's observed denial")
	}
	oldCondition := oldRun.Conditions[0]
	if !oldCondition.AuthorizationObservedAt.Equal(projectBase.Add(time.Second)) || !oldCondition.AvailabilityObservedAt.Equal(projectBase.Add(5*time.Second)) {
		t.Fatalf("wrong evidence clocks: %+v", oldCondition)
	}
	for _, order := range [][2]RequiredMCPCondition{{oldCondition, newRun.Conditions[0]}, {newRun.Conditions[0], oldCondition}} {
		combined := MergeRequiredMCPCondition(order[0], order[1])
		if combined.Active || !combined.AuthorizationObservedAt.Equal(projectBase.Add(3*time.Second)) {
			t.Fatalf("old run relatched recovered denial: %+v", combined)
		}
	}
}
