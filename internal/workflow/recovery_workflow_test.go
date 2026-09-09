package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestReferenceRecoveryPreservesImplementationGates(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")
	readWorkflow := func(name string) apiv1.Workflow {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, "workflows", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var result apiv1.Workflow
		if err := yaml.UnmarshalStrict(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	base, recovery := readWorkflow("implementation"), readWorkflow("implementation-recovery")
	if !reflect.DeepEqual(base.Spec.Gates, recovery.Spec.Gates) || !reflect.DeepEqual(base.Spec.Readiness, recovery.Spec.Readiness) || base.Spec.Start != recovery.Spec.Start {
		t.Fatal("recovery changed implementation gates, admission budgets, or credential preflight")
	}
	if len(recovery.Spec.Tasks) != len(base.Spec.Tasks)+1 {
		t.Fatal("recovery must add exactly one restoration stage")
	}
	tasks := make(map[string]apiv1.Task)
	for _, task := range recovery.Spec.Tasks {
		tasks[task.Name] = task
	}
	for _, task := range base.Spec.Tasks {
		got, ok := tasks[task.Name]
		if !ok {
			t.Fatalf("recovery dropped %s", task.Name)
		}
		if task.Name == "query-backlog" {
			if got.Next != "recovery-resume" || got.Inputs["trustLabel"] != "goobers:approved" || got.Inputs["requireLabels"] != "goobers:needs-remediation" || got.Inputs["filterParkLabels"] != "false" || got.Inputs["excludeLabels"] != "goobers:needs-human,goobers:blocked-on-sibling,goobers/status:in-review" {
				t.Fatal("recovery selector lost trust, parked selection, exclusions or restore-before-implementation")
			}
			// Only selection and its immediate successor may differ. The claim
			// command, open-PR read capability and result contract stay intact.
			got.Goal, got.Next, got.Inputs = task.Goal, task.Next, task.Inputs
		}
		if !reflect.DeepEqual(got, task) {
			t.Fatalf("recovery changed implementation task %s", task.Name)
		}
	}
	restore := tasks["recovery-resume"]
	if restore.Run == nil || !reflect.DeepEqual(restore.Run.Command, []string{"goobers", "recovery-resume"}) || restore.Next != "gather-implement-context" || restore.Type != apiv1.TaskDeterministic || restore.TimeoutSeconds != 360 || !reflect.DeepEqual(restore.Capabilities, []string{"repo:push"}) {
		t.Fatal("recovery stage is not a bounded deterministic restoration before implementation")
	}
	goobers := make(map[string]apiv1.GooberSpec)
	for _, name := range []string{"implementer", "reviewer"} {
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var goober apiv1.Goober
		if err := yaml.UnmarshalStrict(raw, &goober); err != nil {
			t.Fatal(err)
		}
		goobers[goober.Name] = goober.Spec
	}
	if _, err := compileAcknowledged(Definition{Name: recovery.Name, Version: 1, Spec: recovery.Spec}, WithGoobers(goobers)); err != nil {
		t.Fatalf("recovery workflow does not compile: %v", err)
	}
}
