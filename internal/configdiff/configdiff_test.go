package configdiff

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOperationalFields(t *testing.T) {
	want := []string{
		"spec.triggers[] (presence/enablement)",
		"spec.triggers[].schedule",
		"spec.readiness.maxConcurrentRuns",
		"spec.readiness.maxRunsPerHour",
		"spec.readiness.maxRunsPerDay",
		"spec.readiness.maxOpenPRs",
	}
	if got := OperationalFields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("OperationalFields() = %#v, want %#v", got, want)
	}
}

func TestCompareOperationalDifferencesAreInformational(t *testing.T) {
	canonical := workflowFixture()
	active := cloneWorkflow(t, canonical)
	active.Spec.Triggers[0].Schedule = "0 * * * *"
	active.Spec.Triggers = append(active.Spec.Triggers, apiv1.Trigger{Type: apiv1.TriggerManual})
	active.Spec.Readiness = apiv1.ReadinessConditions{
		MaxConcurrentRuns: 4,
		MaxRunsPerHour:    8,
		MaxRunsPerDay:     12,
		MaxOpenPRs:        3,
	}

	differences, err := Compare([]apiv1.Workflow{active}, []apiv1.Workflow{canonical})
	if err != nil {
		t.Fatal(err)
	}
	if len(differences) != 6 {
		t.Fatalf("differences = %+v, want six operational differences", differences)
	}
	for _, difference := range differences {
		if difference.Severity != Informational {
			t.Errorf("difference = %+v, want informational", difference)
		}
	}
}

func TestCompareDisablingOneSameTypeTriggerIsInformational(t *testing.T) {
	canonical := workflowFixture()
	canonical.Spec.Triggers = []apiv1.Trigger{
		{Type: apiv1.TriggerSignal, Signal: "merged"},
		{Type: apiv1.TriggerSignal, Signal: "ready"},
	}
	active := cloneWorkflow(t, canonical)
	active.Spec.Triggers = active.Spec.Triggers[1:]

	differences, err := Compare([]apiv1.Workflow{active}, []apiv1.Workflow{canonical})
	if err != nil {
		t.Fatal(err)
	}
	if len(differences) != 1 ||
		differences[0].Severity != Informational ||
		differences[0].SubjectKind != "trigger" ||
		differences[0].Active != "<missing>" {
		t.Fatalf("differences = %+v, want one informational disabled trigger", differences)
	}
}

func TestCompareChangingSameTypeTriggerIsStructural(t *testing.T) {
	canonical := workflowFixture()
	canonical.Spec.Triggers = []apiv1.Trigger{
		{Type: apiv1.TriggerSignal, Signal: "merged"},
		{Type: apiv1.TriggerSignal, Signal: "ready"},
	}
	active := cloneWorkflow(t, canonical)
	active.Spec.Triggers[0].Signal = "closed"

	differences, err := Compare([]apiv1.Workflow{active}, []apiv1.Workflow{canonical})
	if err != nil {
		t.Fatal(err)
	}
	if len(differences) != 1 ||
		differences[0].Severity != Error ||
		differences[0].SubjectKind != "trigger" ||
		differences[0].Field != "signal" {
		t.Fatalf("differences = %+v, want one structural signal difference", differences)
	}
}

func TestCompareStructuralDifferencesIdentifySubjectsAndValues(t *testing.T) {
	canonical := workflowFixture()
	active := cloneWorkflow(t, canonical)
	delete(active.Spec.Tasks[0].Inputs, "resultFile")
	active.Spec.Tasks[0].Run.Command[1] = "other-stage"
	active.Spec.Tasks[0].Next = "done"
	active.Spec.Gates[0].Branches["pass"] = "done"

	differences, err := Compare([]apiv1.Workflow{active}, []apiv1.Workflow{canonical})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"task:query:inputs.resultFile": {"<missing>", `"claimed-item.json"`},
		"task:query:next":              {`"done"`, `"review"`},
		"task:query:run.command[1]":    {`"other-stage"`, `"backlog-query"`},
		"gate:review:branches.pass":    {`"done"`, `"finish"`},
	}
	if len(differences) != len(want) {
		t.Fatalf("differences = %+v, want %d structural differences", differences, len(want))
	}
	for _, difference := range differences {
		if difference.Severity != Error {
			t.Errorf("difference = %+v, want error", difference)
		}
		key := difference.SubjectKind + ":" + difference.Subject + ":" + difference.Field
		values, ok := want[key]
		if !ok {
			t.Errorf("unexpected difference %+v", difference)
			continue
		}
		if difference.Workflow != "goobers/implementation" {
			t.Errorf("workflow = %q, want goobers/implementation", difference.Workflow)
		}
		if difference.Active != values[0] || difference.Canonical != values[1] {
			t.Errorf("%s values = (%s, %s), want (%s, %s)", key, difference.Active, difference.Canonical, values[0], values[1])
		}
	}
}

func TestCompareRequiredStructuralFieldsAreErrors(t *testing.T) {
	tests := []struct {
		name        string
		change      func(*apiv1.Workflow)
		subjectKind string
		fieldPrefix string
	}{
		{
			name: "task set",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks = nil
			},
			subjectKind: "task",
			fieldPrefix: "<definition>",
		},
		{
			name: "command",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks[0].Run.Command[0] = "other"
			},
			subjectKind: "task",
			fieldPrefix: "run.command",
		},
		{
			name: "inputs",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks[0].Inputs["resultFile"] = "other.json"
			},
			subjectKind: "task",
			fieldPrefix: "inputs.resultFile",
		},
		{
			name: "inputs from",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks[0].InputsFrom["item"] = "other"
			},
			subjectKind: "task",
			fieldPrefix: "inputsFrom.item",
		},
		{
			name: "expected outputs",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks[0].ExpectedOutputs[0] = "other"
			},
			subjectKind: "task",
			fieldPrefix: "expectedOutputs",
		},
		{
			name: "capabilities",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks[0].Capabilities[0] = "repo:push"
			},
			subjectKind: "task",
			fieldPrefix: "capabilities",
		},
		{
			name: "gate set",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Gates = nil
			},
			subjectKind: "gate",
			fieldPrefix: "<definition>",
		},
		{
			name: "branch target",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Gates[0].Branches["pass"] = "other"
			},
			subjectKind: "gate",
			fieldPrefix: "branches.pass",
		},
		{
			name: "next routing",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Tasks[0].Next = "other"
			},
			subjectKind: "task",
			fieldPrefix: "next",
		},
		{
			name: "trigger selector",
			change: func(workflow *apiv1.Workflow) {
				workflow.Spec.Triggers[1].Signal = "other"
			},
			subjectKind: "trigger",
			fieldPrefix: "signal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canonical := workflowFixture()
			active := cloneWorkflow(t, canonical)
			test.change(&active)

			differences, err := Compare([]apiv1.Workflow{active}, []apiv1.Workflow{canonical})
			if err != nil {
				t.Fatal(err)
			}
			if len(differences) != 1 {
				t.Fatalf("differences = %+v, want exactly one", differences)
			}
			difference := differences[0]
			if difference.Severity != Error ||
				difference.SubjectKind != test.subjectKind ||
				!strings.HasPrefix(difference.Field, test.fieldPrefix) {
				t.Fatalf("difference = %+v, want error %s field %s", difference, test.subjectKind, test.fieldPrefix)
			}
		})
	}
}

func TestCompareMissingWorkflowIsStructural(t *testing.T) {
	canonical := workflowFixture()
	differences, err := Compare(nil, []apiv1.Workflow{canonical})
	if err != nil {
		t.Fatal(err)
	}
	if len(differences) != 1 {
		t.Fatalf("differences = %+v, want exactly one", differences)
	}
	difference := differences[0]
	if difference.Severity != Error ||
		difference.Workflow != "goobers/implementation" ||
		difference.Field != "<definition>" ||
		difference.Active != "<missing>" ||
		difference.Canonical == "<missing>" {
		t.Fatalf("difference = %+v, want missing active workflow with canonical value", difference)
	}
}

func TestCompareIgnoresSetAndDefinitionOrdering(t *testing.T) {
	canonical := workflowFixture()
	canonical.Spec.Tasks = append(canonical.Spec.Tasks, apiv1.Task{
		Name: "finish",
		Type: apiv1.TaskDeterministic,
		Goal: "Finish.",
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
	})
	active := cloneWorkflow(t, canonical)
	active.Spec.Tasks[0], active.Spec.Tasks[1] = active.Spec.Tasks[1], active.Spec.Tasks[0]
	active.Spec.Tasks[1].Capabilities[0], active.Spec.Tasks[1].Capabilities[1] =
		active.Spec.Tasks[1].Capabilities[1], active.Spec.Tasks[1].Capabilities[0]
	active.Spec.Triggers[0], active.Spec.Triggers[1] = active.Spec.Triggers[1], active.Spec.Triggers[0]

	differences, err := Compare([]apiv1.Workflow{active}, []apiv1.Workflow{canonical})
	if err != nil {
		t.Fatal(err)
	}
	if len(differences) != 0 {
		t.Fatalf("ordering produced differences: %+v", differences)
	}
}

func workflowFixture() apiv1.Workflow {
	return apiv1.Workflow{
		TypeMeta: metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Workflow"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "implementation",
		},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers",
			Triggers: []apiv1.Trigger{
				{Type: apiv1.TriggerSchedule, Schedule: "30 * * * *"},
				{Type: apiv1.TriggerSignal, Signal: "ready"},
			},
			Readiness: apiv1.ReadinessConditions{
				MaxConcurrentRuns: 1,
				MaxRunsPerHour:    2,
				MaxRunsPerDay:     4,
				MaxOpenPRs:        1,
			},
			Start: "query",
			Tasks: []apiv1.Task{{
				Name:            "query",
				Type:            apiv1.TaskDeterministic,
				Goal:            "Query backlog.",
				Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}},
				Inputs:          map[string]string{"resultFile": "claimed-item.json"},
				InputsFrom:      map[string]string{"item": "claimed-item"},
				Capabilities:    []string{"github:pr:write", "github:issues:write"},
				ExpectedOutputs: []string{"claimed-item"},
				Next:            "review",
			}},
			Gates: []apiv1.Gate{{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": "finish", "fail": "@fail"},
			}},
		},
	}
}

func cloneWorkflow(t *testing.T, workflow apiv1.Workflow) apiv1.Workflow {
	t.Helper()
	data, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	var cloned apiv1.Workflow
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}
