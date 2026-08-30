package backlogdefaults

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// backlogQueryTask is the shape both defaults key on.
var backlogQueryTask = apiv1.Task{
	Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
}

// TestAssignedTo mirrors internal/runner/selfidentity_test.go case for case.
// The two implementations are copies (see the package comment), so their tests
// are copies too: a case that exists on one side and not the other is where
// drift starts.
func TestAssignedTo(t *testing.T) {
	tests := []struct {
		name       string
		task       apiv1.Task
		inputs     map[string]string
		configured string
		want       map[string]string
	}{
		{
			name:       "configured default",
			task:       backlogQueryTask,
			inputs:     map[string]string{"trustLabel": "goobers:approved"},
			configured: "gaggle-bot",
			want:       map[string]string{"trustLabel": "goobers:approved", "assignedTo": "gaggle-bot"},
		},
		{
			name:       "configured default with no declared inputs",
			task:       backlogQueryTask,
			configured: "gaggle-bot",
			want:       map[string]string{"assignedTo": "gaggle-bot"},
		},
		{
			name:       "explicit override",
			task:       backlogQueryTask,
			inputs:     map[string]string{"assignedTo": "invocation-user"},
			configured: "gaggle-bot",
			want:       map[string]string{"assignedTo": "invocation-user"},
		},
		{
			name:   "absent stays opted out",
			task:   backlogQueryTask,
			inputs: map[string]string{"trustLabel": "goobers:approved"},
			want:   map[string]string{"trustLabel": "goobers:approved"},
		},
		{
			name:       "unrelated task unchanged",
			task:       apiv1.Task{Run: &apiv1.DeterministicRun{Command: []string{"goobers", "open-pr"}}},
			inputs:     map[string]string{},
			configured: "gaggle-bot",
			want:       map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssignedTo(tt.task, tt.inputs, tt.configured)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("inputs = %#v, want %#v", got, tt.want)
			}
		})
	}

	declared := map[string]string{"trustLabel": "goobers:approved"}
	AssignedTo(backlogQueryTask, declared, "gaggle-bot")
	if !reflect.DeepEqual(declared, map[string]string{"trustLabel": "goobers:approved"}) {
		t.Fatalf("declared task inputs were mutated: %#v", declared)
	}
}

// TestRequireLabels mirrors internal/runner/requirelabels_test.go case for
// case.
func TestRequireLabels(t *testing.T) {
	tests := []struct {
		name       string
		task       apiv1.Task
		inputs     map[string]string
		configured string
		want       map[string]string
	}{
		{
			name:       "configured default",
			task:       backlogQueryTask,
			inputs:     map[string]string{"trustLabel": "goobers:approved"},
			configured: "goobers:ready,area:frontend",
			want:       map[string]string{"trustLabel": "goobers:approved", "requireLabels": "goobers:ready,area:frontend"},
		},
		{
			name:       "configured default with no declared inputs",
			task:       backlogQueryTask,
			configured: "area:frontend",
			want:       map[string]string{"requireLabels": "area:frontend"},
		},
		{
			name:       "explicit override",
			task:       backlogQueryTask,
			inputs:     map[string]string{"requireLabels": "goobers:ready,area:billing"},
			configured: "area:frontend",
			want:       map[string]string{"requireLabels": "goobers:ready,area:billing"},
		},
		{
			name:   "absent stays opted out",
			task:   backlogQueryTask,
			inputs: map[string]string{"trustLabel": "goobers:approved"},
			want:   map[string]string{"trustLabel": "goobers:approved"},
		},
		{
			name:       "unrelated task unchanged",
			task:       apiv1.Task{Run: &apiv1.DeterministicRun{Command: []string{"goobers", "open-pr"}}},
			inputs:     map[string]string{},
			configured: "area:frontend",
			want:       map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequireLabels(tt.task, tt.inputs, tt.configured)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("inputs = %#v, want %#v", got, tt.want)
			}
		})
	}

	declared := map[string]string{"trustLabel": "goobers:approved"}
	RequireLabels(backlogQueryTask, declared, "area:frontend")
	if !reflect.DeepEqual(declared, map[string]string{"trustLabel": "goobers:approved"}) {
		t.Fatalf("declared task inputs were mutated: %#v", declared)
	}
}

// TestApplyAppliesBothDefaults pins the composite a driver actually calls: one
// call, both partitions. A driver that applied only the identity would claim
// the sibling instance's items (#3873); one that applied only the labels would
// claim its own items under nobody's identity.
func TestApplyAppliesBothDefaults(t *testing.T) {
	got := Apply(backlogQueryTask, map[string]string{"trustLabel": "goobers:approved"}, "goobersbot", "goobers:cloud")
	want := map[string]string{
		"trustLabel":    "goobers:approved",
		"assignedTo":    "goobersbot",
		"requireLabels": "goobers:cloud",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inputs = %#v, want %#v", got, want)
	}
}

// TestApplyLeavesDeclaredInputsAlone is the over-application guard: a stage
// that names its own claim query keeps it, whatever the gaggle is configured
// with. Blanket-stamping the gaggle identity onto every backlog-query stage
// would retarget the implementation lane's goobers:ready claim.
func TestApplyLeavesDeclaredInputsAlone(t *testing.T) {
	declared := map[string]string{
		"requireLabels": "goobers:ready",
		"assignedTo":    "declared-claimer",
	}
	got := Apply(backlogQueryTask, declared, "goobersbot", "goobers:cloud")
	if !reflect.DeepEqual(got, declared) {
		t.Fatalf("inputs = %#v, want the declared pair %#v untouched", got, declared)
	}
}

// TestApplyIsANoOpWithoutDefaults pins the zero-configuration invariance every
// type-1/type-2 instance depends on: no gaggle defaults means the SAME map
// back, not a copy of it, byte for byte as before this package existed.
func TestApplyIsANoOpWithoutDefaults(t *testing.T) {
	inputs := map[string]string{"trustLabel": "goobers:approved"}
	got := Apply(backlogQueryTask, inputs, "", "")
	if !reflect.DeepEqual(got, inputs) {
		t.Fatalf("inputs = %#v, want %#v", got, inputs)
	}
	got["marker"] = "written-through"
	if _, ok := inputs["marker"]; !ok {
		t.Fatal("Apply copied the inputs map for a no-op; the runner returns the same map")
	}
}

// TestIsBacklogQuery pins the predicate both defaults share, including the
// absolute-path form a workspace-materialized command takes and the agentic
// stage that has no Run block at all.
func TestIsBacklogQuery(t *testing.T) {
	tests := []struct {
		name string
		task apiv1.Task
		want bool
	}{
		{name: "claim", task: backlogQueryTask, want: true},
		{
			name: "absolute path",
			task: apiv1.Task{Run: &apiv1.DeterministicRun{Command: []string{"/usr/local/bin/goobers", "backlog-query"}}},
			want: true,
		},
		{
			name: "other subcommand",
			task: apiv1.Task{Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-health"}}},
		},
		{
			name: "other binary",
			task: apiv1.Task{Run: &apiv1.DeterministicRun{Command: []string{"gh", "backlog-query"}}},
		},
		{
			name: "no subcommand",
			task: apiv1.Task{Run: &apiv1.DeterministicRun{Command: []string{"goobers"}}},
		},
		{name: "agentic stage has no run block", task: apiv1.Task{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBacklogQuery(tt.task); got != tt.want {
				t.Fatalf("IsBacklogQuery = %t, want %t", got, tt.want)
			}
		})
	}
}
