package runner

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDefaultBacklogQueryRequireLabels(t *testing.T) {
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
			configured: "goobers:ready,area:frontend",
			want:       map[string]string{"trustLabel": "goobers:approved", "requireLabels": "goobers:ready,area:frontend"},
		},
		{
			name:       "configured default with no declared inputs",
			task:       backlogQuery,
			configured: "area:frontend",
			want:       map[string]string{"requireLabels": "area:frontend"},
		},
		{
			name:       "explicit override",
			task:       backlogQuery,
			inputs:     map[string]string{"requireLabels": "goobers:ready,area:billing"},
			configured: "area:frontend",
			want:       map[string]string{"requireLabels": "goobers:ready,area:billing"},
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
			configured: "area:frontend",
			want:       map[string]string{},
		},
		{
			// #4180: backlog-health reads the same partitioned backlog as
			// backlog-query, and without this default its ready-pool
			// measurement counts items outside the gaggle's partition.
			name: "backlog-health task also receives the default",
			task: apiv1.Task{
				Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-health"}},
			},
			inputs:     map[string]string{"trustLabel": "goobers:approved"},
			configured: "goobers:cloud",
			want:       map[string]string{"trustLabel": "goobers:approved", "requireLabels": "goobers:cloud"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultBacklogQueryRequireLabels(tt.task, tt.inputs, tt.configured)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("inputs = %#v, want %#v", got, tt.want)
			}
		})
	}

	declared := map[string]string{"trustLabel": "goobers:approved"}
	defaultBacklogQueryRequireLabels(backlogQuery, declared, "area:frontend")
	if !reflect.DeepEqual(declared, map[string]string{"trustLabel": "goobers:approved"}) {
		t.Fatalf("declared task inputs were mutated: %#v", declared)
	}
}
