package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

func fleetMCPTestObservation(at time.Time, category string) journal.Event {
	authorization := "ready"
	if category == "check_unobservable" {
		authorization = "unobservable"
	}
	if category == "tool_authorization_failure" {
		authorization = "denied"
	}
	return journal.Event{Schema: journal.EventSchema, Type: journal.EventRunnerAnnotation, Stage: "implement", Time: at, Runner: map[string]any{"kind": "required-mcp-readiness", "schemaVersion": 1, "adapter": "copilot-cli", "server": "goobers-io", "category": category, "connection": "ready", "inventory": "ready", "authorization": authorization}}
}
func fleetMCPTestRun(events ...journal.Event) readservice.RunSummary {
	var state *readmodel.RequiredMCPState
	for _, event := range events {
		state = state.After(event)
	}
	return readservice.RunSummary{Gaggle: "alpha", Workflow: "implementation", RequiredMCP: state}
}

func TestFleetMCPHealthCrossRunRecoveryAndIndependentClocks(t *testing.T) {
	now := time.Now().UTC()
	old := fleetMCPTestRun(fleetMCPTestObservation(now.Add(-5*time.Minute), "tool_authorization_failure"), fleetMCPTestObservation(now, "check_unobservable"))
	health := fleetMCPHealth([]readservice.RunSummary{old}, "alpha", true, now)
	if health.State != "active" || health.Reason != "tool_authorization_failure" || !health.ObservedAt.Equal(now.Add(-5*time.Minute)) {
		t.Fatal(health)
	}
	recovered := fleetMCPTestRun(fleetMCPTestObservation(now.Add(-time.Minute), "ready"))
	for _, runs := range [][]readservice.RunSummary{{old, recovered}, {recovered, old}} {
		health = fleetMCPHealth(runs, "alpha", true, now)
		if health.State != "recovered" || health.ActiveCount == nil || *health.ActiveCount != 0 {
			t.Fatal(health)
		}
		health = fleetMCPHealth(runs, "alpha", false, now)
		if health.State != "unknown" || health.ActiveCount != nil {
			t.Fatal("partial history claimed recovery", health)
		}
	}
	recovered.Workflow = "other"
	health = fleetMCPHealth([]readservice.RunSummary{old, recovered}, "alpha", true, now)
	if health.State != "active" || health.Workflow != "implementation" {
		t.Fatal("other workflow cleared condition", health)
	}
}

func TestFleetMCPHealthNativeContractAndUnknowns(t *testing.T) {
	now := time.Now().UTC()
	for _, category := range []string{"ready", "check_unobservable", "transport_failure", "tool_authorization_failure"} {
		health := fleetMCPHealth([]readservice.RunSummary{fleetMCPTestRun(fleetMCPTestObservation(now, category))}, "alpha", true, now)
		observer := fleetHealthObserver{instanceID: "instance", bootID: "boot", startedAt: now.Add(-time.Minute), sequence: 1}
		attrs := observer.identity("alpha", now)
		attrs["state"], attrs["reasonCode"], attrs["windowCoverage"] = "productive", "progress_observed", "complete"
		fleetMCPAttributes(attrs, health)
		decoded, err := fleetdiagnostics.DecodeHeartbeat(attrs)
		if err != nil || decoded.RequiredMCP == nil || decoded.RequiredMCP.State != health.State {
			t.Fatalf("%s: %+v %v", category, decoded, err)
		}
	}
	future := fleetMCPTestRun(fleetMCPTestObservation(now.Add(time.Minute), "ready"))
	if got := fleetMCPHealth([]readservice.RunSummary{future}, "alpha", true, now); got.State != "unknown" || got.ActiveCount != nil {
		t.Fatal(got)
	}
}

func TestFleetMCPEqualTimeDenialDoesNotDisappear(t *testing.T) {
	now := time.Now().UTC()
	denied := fleetMCPTestRun(fleetMCPTestObservation(now, "tool_authorization_failure"))
	ready := fleetMCPTestRun(fleetMCPTestObservation(now, "ready"))
	for _, runs := range [][]readservice.RunSummary{{denied, ready}, {ready, denied}} {
		if health := fleetMCPHealth(runs, "alpha", true, now); health.State != "active" {
			t.Fatal(health)
		}
	}
}

func TestFleetMCPContextBoundAndTruncatedEvidence(t *testing.T) {
	now := time.Now().UTC()
	runs := make([]readservice.RunSummary, fleetMCPContextLimit+1)
	for i := range runs {
		runs[i] = fleetMCPTestRun(fleetMCPTestObservation(now, "tool_authorization_failure"))
		runs[i].RequiredMCP.Conditions[0].Branch = i
	}
	health := fleetMCPHealth(runs, "alpha", true, now)
	if health.Coverage != "partial" || health.ActiveCount == nil || *health.ActiveCount != fleetMCPContextLimit {
		t.Fatal(health)
	}
	ready := fleetMCPTestRun(fleetMCPTestObservation(now, "ready"))
	ready.RequiredMCP.Truncated = true
	if health := fleetMCPHealth([]readservice.RunSummary{ready}, "alpha", true, now); health.State != "unknown" || health.ActiveCount != nil {
		t.Fatal(health)
	}
}

func TestFleetMCPLegacyHistoryCannotClaimCompleteRecovery(t *testing.T) {
	now := time.Now().UTC()
	ready := fleetMCPTestRun(fleetMCPTestObservation(now, "ready"))
	legacy := readservice.RunSummary{Gaggle: "alpha", Workflow: "legacy"}
	health := fleetMCPHealth([]readservice.RunSummary{ready, legacy}, "alpha", true, now)
	if health.State != "unknown" || health.Coverage != "partial" || health.ActiveCount != nil {
		t.Fatal(health)
	}
}
