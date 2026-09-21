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
