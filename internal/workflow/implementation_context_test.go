package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestImplementationWorkflowsGatherFirstPassContext(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "workflows", "implementation.yaml"),
		filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows", "implementation.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			var workflow apiv1.Workflow
			if err := yaml.Unmarshal(raw, &workflow); err != nil {
				t.Fatalf("unmarshal workflow: %v", err)
			}

			tasks := make(map[string]apiv1.Task, len(workflow.Spec.Tasks))
			for _, task := range workflow.Spec.Tasks {
				tasks[task.Name] = task
			}
			query := tasks["query-backlog"]
			if query.Next != "gather-implement-context" {
				t.Fatalf("query-backlog.next = %q, want gather-implement-context", query.Next)
			}
			gather, ok := tasks["gather-implement-context"]
			if !ok {
				t.Fatal("gather-implement-context task not found")
			}
			if gather.Type != apiv1.TaskDeterministic || gather.Run == nil ||
				!reflect.DeepEqual(gather.Run.Command, []string{"goobers", "gather-implement-context"}) {
				t.Fatalf("gather-implement-context task = %+v, want deterministic built-in command", gather)
			}
			if gather.Inputs["resultFile"] != "implementation-context.json" || gather.Inputs["maxHotFiles"] != "100" {
				t.Fatalf("gather-implement-context inputs = %v, want bounded declared result", gather.Inputs)
			}
			if !reflect.DeepEqual(gather.Capabilities, []string{"github:pr:write", "journal:read"}) {
				t.Fatalf("gather-implement-context capabilities = %v, want [github:pr:write journal:read]", gather.Capabilities)
			}
			if gather.Next != "implement" {
				t.Fatalf("gather-implement-context.next = %q, want implement", gather.Next)
			}
		})
	}
}
