package readservice

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDiagnosticFeatureConfigurationTracksAdmittedReload(t *testing.T) {
	initial := inventoryDefinitions()
	service, _ := newInventoryService(t, initial, nil)
	first := service.DiagnosticFeatureConfiguration()
	if !first["alpha"]["adapter.copilot"] || first["alpha"]["adapter.codex"] || !first["alpha"]["provider.github"] {
		t.Fatal(first)
	}
	if _, known := first["alpha"]["runner.engine"]; known {
		t.Fatal("definition config inferred effective driver")
	}
	first["alpha"]["adapter.copilot"] = false
	if !service.DiagnosticFeatureConfiguration()["alpha"]["adapter.copilot"] {
		t.Fatal("caller mutated shared snapshot")
	}
	next := inventoryDefinitions()
	next.Goobers[0].Spec.Harness = apiv1.HarnessCodex
	if err := service.ReloadDefinitions(next, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := service.DiagnosticFeatureConfiguration()
	if got["alpha"]["adapter.copilot"] || !got["alpha"]["adapter.codex"] {
		t.Fatal("reload not reflected", got)
	}
	if err := service.ReloadDefinitions(nil, nil, time.Now()); err == nil {
		t.Fatal("invalid reload accepted")
	}
	if !service.DiagnosticFeatureConfiguration()["alpha"]["adapter.codex"] {
		t.Fatal("rejected config replaced admitted labels")
	}
}

// Portal/API configuration accounting must not disappear when an admitted
// workflow opts into 2.1. Reload changes the snapshot without changing its
// major-version feature identity or classifying it as DSL 3.
func TestDiagnosticFeatureConfigurationCountsDSL21AsV2(t *testing.T) {
	initial := inventoryDefinitions()
	for i := range initial.Workflows {
		initial.Workflows[i].DSLVersion = "2.0"
	}
	service, _ := newInventoryService(t, initial, nil)
	next := inventoryDefinitions()
	for i := range next.Workflows {
		next.Workflows[i].DSLVersion = "2.1"
	}
	if err := service.ReloadDefinitions(next, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := service.DiagnosticFeatureConfiguration()
	for _, gaggle := range []string{"alpha", "beta"} {
		if !got[gaggle]["dsl.v2"] || got[gaggle]["dsl.v1"] || got[gaggle]["dsl.v3"] {
			t.Fatalf("%s lost DSL 2 accounting after 2.1 reload: %v", gaggle, got[gaggle])
		}
	}
}
