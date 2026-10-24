package main

import (
	"testing"

	"github.com/goobers/goobers/internal/localscheduler"
)

func TestAdmittedFeatureDriverSnapshot(t *testing.T) {
	entries := []localscheduler.WorkflowEntry{{Gaggle: "alpha", Workflow: "local", Starter: &trackedStarter{}}, {Gaggle: "beta", Workflow: "unknown"}}
	definitions := interventionDefinitions(&schedulerDefinitions{Entries: entries}, nil)
	registry := newInterventionDefinitionRegistry(definitions)
	got := registry.Snapshot().featureDrivers
	if got[localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "local"}] != "runner.local" || got[localscheduler.WorkflowIdentity{Gaggle: "beta", Workflow: "unknown"}] != "" {
		t.Fatal(got)
	}
	entries[0].Starter = nil
	registry.Replace(interventionDefinitions(&schedulerDefinitions{Entries: entries}, nil))
	if registry.Snapshot().featureDrivers[localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "local"}] != "" {
		t.Fatal("accepted snapshot did not replace effective driver")
	}
	if got[localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "local"}] != "runner.local" {
		t.Fatal("old snapshot mutated")
	}
	if len(featureDriverConfiguration(make([]localscheduler.WorkflowEntry, 1001))) != 0 {
		t.Fatal("oversized inventory claimed coverage")
	}
}
