package main

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/workflow"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// TestRegisteredThirdHarnessRequiresModelCapability exercises the production
// registry-to-compiler seam. A newly registered adapter must not bypass model
// credential admission merely because its name is not a built-in constant.
func TestRegisteredThirdHarnessRequiresModelCapability(t *testing.T) {
	registry := harness.NewRegistry()
	if err := registry.RegisterAs("third-adapter", &harnesstest.FakeAdapter{}); err != nil {
		t.Fatal(err)
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "example",
		Start:  "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement",
		}},
	}
	_, err := workflow.Compile(
		workflow.Definition{Name: "third-harness", Version: 1, DSLVersion: "2.0", Spec: spec},
		workflow.WithGoobers(map[string]apiv1.GooberSpec{
			"coder": {Role: "coder", Harness: apiv1.Harness("third-adapter")},
		}),
		workflow.WithKnownHarnesses(registry.Names()),
	)
	if err == nil || !strings.Contains(err.Error(), "third-adapter") || !strings.Contains(err.Error(), "agent:model") {
		t.Fatalf("Compile() error = %v, want registered third harness model-capability refusal", err)
	}
	if strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("Compile() error = %v, registered third adapter was treated as unknown", err)
	}

	spec.Tasks[0].Capabilities = []string{string(capability.AgentModel)}
	if _, err := workflow.Compile(
		workflow.Definition{Name: "third-harness", Version: 1, DSLVersion: "2.0", Spec: spec},
		workflow.WithGoobers(map[string]apiv1.GooberSpec{
			"coder": {
				Role:         "coder",
				Harness:      apiv1.Harness("third-adapter"),
				Capabilities: []string{string(capability.AgentModel)},
			},
		}),
		workflow.WithKnownHarnesses(registry.Names()),
	); err != nil {
		t.Fatalf("Compile() with registered third harness model capability: %v", err)
	}
}
