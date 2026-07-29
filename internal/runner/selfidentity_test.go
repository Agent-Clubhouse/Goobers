package runner

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDefaultBacklogQueryAssignedTo(t *testing.T) {
	backlogQuery := apiv1.Task{
		Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
	}

	tests := []struct {
		name       string
		task       apiv1.Task
		inputs     map[string]string
		configured string
		want       map[string]string
	}{
		{
			name:       "configured default",
			task:       backlogQuery,
			inputs:     map[string]string{"trustLabel": "goobers:approved"},
			configured: "gaggle-bot",
			want:       map[string]string{"trustLabel": "goobers:approved", "assignedTo": "gaggle-bot"},
		},
		{
			name:       "configured default with no declared inputs",
			task:       backlogQuery,
			configured: "gaggle-bot",
			want:       map[string]string{"assignedTo": "gaggle-bot"},
		},
		{
			name:       "explicit override",
			task:       backlogQuery,
			inputs:     map[string]string{"assignedTo": "invocation-user"},
			configured: "gaggle-bot",
			want:       map[string]string{"assignedTo": "invocation-user"},
		},
		{
			name:   "absent stays opted out",
			task:   backlogQuery,
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
			got := defaultBacklogQueryAssignedTo(tt.task, tt.inputs, tt.configured)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("inputs = %#v, want %#v", got, tt.want)
			}
		})
	}

	declared := map[string]string{"trustLabel": "goobers:approved"}
	defaultBacklogQueryAssignedTo(backlogQuery, declared, "gaggle-bot")
	if !reflect.DeepEqual(declared, map[string]string{"trustLabel": "goobers:approved"}) {
		t.Fatalf("declared task inputs were mutated: %#v", declared)
	}
}
