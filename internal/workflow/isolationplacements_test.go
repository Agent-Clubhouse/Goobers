package workflow

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestIsolationStagePlacementsCoverUnplacedGatesWithoutChangingLegacyRows(t *testing.T) {
	def := Definition{Name: "wf", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{
		Tasks: []apiv1.Task{{Name: "agent", Type: apiv1.TaskAgentic}, {Name: "script", Type: apiv1.TaskDeterministic}},
		Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic}, {Name: "check", Evaluator: apiv1.EvaluatorAutomated}},
	}}
	baseline, err := StagePlacements(def, apiv1.GaggleSpec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := IsolationStagePlacements(def, apiv1.GaggleSpec{}, nil, nil)
	if err != nil || !reflect.DeepEqual(baseline, unchanged) {
		t.Fatalf("legacy rows changed: %v, %v", unchanged, err)
	}
	rows, err := IsolationStagePlacements(def, apiv1.GaggleSpec{}, nil, map[string][]string{"agentic": {"tmp:ephemeral"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].StageClass != "agentic" || rows[1].StageClass != "deterministic" || rows[2].Stage != "review" || !rows[2].ControlPlane {
		t.Fatalf("policy rows: %+v", rows)
	}
	rows, err = IsolationStagePlacements(def, apiv1.GaggleSpec{}, nil, map[string][]string{"deterministic": {"tmp:ephemeral"}})
	if err != nil || len(rows) != 3 || rows[2].Stage != "check" || !rows[2].ControlPlane {
		t.Fatalf("deterministic gate omitted: %+v, %v", rows, err)
	}
}
