package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
)

func TestChildStageBaseSyncDoesNotAuthorizePodNetworkOrPublication(t *testing.T) {
	for _, scenario := range []string{"sync", "network", "run-context", "publication"} {
		t.Run(scenario, func(t *testing.T) {
			run := &apiv1.DeterministicRun{Command: []string{"true"}, SyncBase: true}
			switch scenario {
			case "network":
				run.Network = "host"
			case "run-context":
				run.InjectRunContext = true
			case "publication":
				run.Command = []string{"goobers", "open-pr"}
			}
			start := childExecutionStart{Proposal: &childworkflow.Proposal{
				Workflow:   apiv1.Workflow{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Run: run}}}},
				Placements: []engine.PinnedPlacement{{Stage: "build"}},
			}}
			err := admitChildPodStages(start)
			if (err == nil) != (scenario == "sync") {
				t.Fatal("host merge changed pod authority", scenario, err)
			}
		})
	}
}
