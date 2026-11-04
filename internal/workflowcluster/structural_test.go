package workflowcluster

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func reviewLoop(names [4]string, goal string) workflow.Definition {
	implement, review, verdict, publish := names[0], names[1], names[2], names[3]
	return workflow.Definition{
		Name: "child", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: implement,
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Tasks: []apiv1.Task{
				{Name: implement, Type: apiv1.TaskAgentic, Goal: goal, Goober: "coder", Next: review,
					Retry: &apiv1.RetryPolicy{MaxAttempts: 3}, TimeoutSeconds: 600},
				{Name: review, Type: apiv1.TaskAgentic, Goal: "review", Goober: "reviewer", Next: verdict},
				{Name: publish, Type: apiv1.TaskDeterministic, Goal: "publish", Capabilities: []string{"pr.create"}},
			},
			Gates: []apiv1.Gate{{
				Name: verdict, Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "review-verdict"},
				Branches:  map[string]string{"approved": publish, "changes": implement},
			}},
		},
	}
}

func extract(t *testing.T, def workflow.Definition) StructuralFeatures {
	t.Helper()
	features, err := ExtractDefinition(def)
	if err != nil {
		t.Fatal(err)
	}
	return features
}

func TestStructuralFeaturesIgnoreNamesOrderAndGoalContent(t *testing.T) {
	base := extract(t, reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "write the fix"))

	renamed := reviewLoop([4]string{"a", "b", "c", "d"}, "fix the write")
	tasks := renamed.Spec.Tasks
	renamed.Spec.Tasks = []apiv1.Task{tasks[2], tasks[0], tasks[1]}
	other := extract(t, renamed)

	if !reflect.DeepEqual(base.Numeric, other.Numeric) || !reflect.DeepEqual(base.WL, other.WL) {
		t.Fatalf("renamed/reordered features differ:\n%v %v\n%v %v", base.Numeric, base.WL, other.Numeric, other.WL)
	}
	if !reflect.DeepEqual(base.Categorical, other.Categorical) {
		t.Fatalf("categorical differs: %v vs %v", base.Categorical, other.Categorical)
	}
	// The digest is the join key to run telemetry, so it must match the runtime pin.
	pin, err := workflow.ComputeDigest(reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "write the fix"))
	if err != nil {
		t.Fatal(err)
	}
	if base.Version != StructuralFeatureVersion || base.WorkflowDigest != pin {
		t.Fatalf("identity = %q/%q, want %q/%q", base.Version, base.WorkflowDigest, StructuralFeatureVersion, pin)
	}
}

func TestStructuralFeaturesCaptureShape(t *testing.T) {
	f := extract(t, reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "fix"))
	want := map[string]float64{
		"nodes": 4, "nodes.agentic": 2, "nodes.deterministic": 1, "nodes.gate": 1,
		"edges.next": 2, "edges.gate": 2, "retry.nodes": 1, "retry.attempts": 3,
		"timeout.maxSeconds": 600, "graph.depth": 3, "graph.unreachable": 0,
		// implement -> review -> verdict -> implement is one cycle of three nodes.
		"graph.cycles": 1, "graph.cycleNodes": 3, "capabilities": 1,
	}
	for name, value := range want {
		if f.Numeric[name] != value {
			t.Errorf("%s = %v, want %v", name, f.Numeric[name], value)
		}
	}
	if got := f.Categorical["goobers"]; !reflect.DeepEqual(got, []string{"coder", "reviewer"}) {
		t.Errorf("goobers = %v", got)
	}
	if len(f.WL) != wlBuckets {
		t.Errorf("WL width = %d", len(f.WL))
	}
}

func TestStructuralFeaturesSeparateDifferentShapes(t *testing.T) {
	loop := extract(t, reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "fix"))

	linear := reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "fix")
	linear.Spec.Gates[0].Branches = map[string]string{"approved": "publish", "changes": ""}
	straight := extract(t, linear)

	if straight.Numeric["graph.cycles"] != 0 || straight.Numeric["edges.terminal"] != 1 {
		t.Fatalf("linear shape not captured: %v", straight.Numeric)
	}
	if reflect.DeepEqual(loop.WL, straight.WL) {
		t.Fatal("WL histogram did not distinguish a review loop from a linear pipeline")
	}
}
