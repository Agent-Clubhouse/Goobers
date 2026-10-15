package readservice

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// TestWorkflowStagesIncludeMaps is TestWorkflowStagesIncludeParallels for a
// DSL 3.1 map: the compiled graph has a node per Spec.Maps entry, so the
// stage list the portal checks against it must too.
func TestWorkflowStagesIncludeMaps(t *testing.T) {
	definitions := inventoryDefinitions()
	scratch := func(name, goal, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: goal,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: next}
	}
	plan := scratch("plan", "select the shard roster", "run-shards")
	plan.Inputs = map[string]string{"artifactManifestFile": "manifest.json"}
	plan.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "shards", MediaType: "application/json"}}
	definitions.Workflows = append(definitions.Workflows, apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sharded-tests",
			Annotations: map[string]string{
				"goobers.dev/purpose":              "Run one shard per roster item",
				workflow.PreviewFeaturesAnnotation: "true",
			},
		},
		DSLVersion: "3.1",
		Spec: apiv1.WorkflowSpec{
			Gaggle:    "alpha",
			Triggers:  []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Start:     "plan",
			Tasks: []apiv1.Task{
				plan,
				scratch("run-shard", "run one shard", workflow.TargetJoin),
				scratch("collate", "collate shards", workflow.TerminalComplete),
			},
			Maps: []apiv1.Map{{
				Name: "run-shards", Source: apiv1.FanOutSource{Task: "plan", Slot: "shards"}, Task: "run-shard",
				FailurePolicy: apiv1.BranchContinueOnError, Join: "collate",
			}},
		},
	})

	service, _ := newInventoryService(t, definitions, nil)
	detail, err := service.Workflow(context.Background(), "alpha", "sharded-tests")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Stages) != len(detail.Graph.Nodes) || detail.StageCount != len(detail.Graph.Nodes) {
		t.Fatalf("stages/graph node count mismatch: stages=%d stageCount=%d graph.Nodes=%d",
			len(detail.Stages), detail.StageCount, len(detail.Graph.Nodes))
	}
	byName := make(map[string]StageDefinition, len(detail.Stages))
	for _, stage := range detail.Stages {
		byName[stage.Name] = stage
	}
	for _, node := range detail.Graph.Nodes {
		if stage, ok := byName[node.ID]; !ok || stage.Kind != node.Kind {
			t.Fatalf("graph node %q kind %q has no matching stage (stage=%+v)", node.ID, node.Kind, stage)
		}
	}
	if fanOut := byName["run-shards"]; fanOut.Kind != workflow.GraphNodeMap {
		t.Fatalf("expected a %q stage for the map, got %+v", workflow.GraphNodeMap, fanOut)
	}
}
