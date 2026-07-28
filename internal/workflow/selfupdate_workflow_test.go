package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSelfhostSelfUpdateDefaultsToTaggedReleases(t *testing.T) {
	path := filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows", "self-update.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var update apiv1.Workflow
	if err := yaml.Unmarshal(raw, &update); err != nil {
		t.Fatal(err)
	}
	def := Definition{Name: update.Name, Version: 1, Spec: update.Spec}
	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("compile self-update workflow: %v", err)
	}
	if len(update.Spec.Triggers) != 1 || update.Spec.Triggers[0].Type != apiv1.TriggerSchedule {
		t.Fatalf("triggers = %+v, want one schedule trigger", update.Spec.Triggers)
	}
	if len(update.Spec.Tasks) != 1 {
		t.Fatalf("tasks = %d, want one deterministic stage", len(update.Spec.Tasks))
	}
	task := update.Spec.Tasks[0]
	if task.Inputs["policy"] != "on-release" {
		t.Fatalf("policy = %q, want on-release", task.Inputs["policy"])
	}
	if _, configured := task.Inputs["healthTimeout"]; configured {
		t.Fatalf("healthTimeout must derive from the instance liveness setting")
	}
	if task.Run == nil || !slices.Equal(task.Run.Command, []string{"goobers", "self-update"}) {
		t.Fatalf("command = %+v", task.Run)
	}
	if !containsString(task.Capabilities, "contents:read") ||
		!containsString(task.Capabilities, "github:issues:write") ||
		!containsString(task.PolicyActions, "create-issue") {
		t.Fatalf("rollback escalation declarations = capabilities %v, actions %v", task.Capabilities, task.PolicyActions)
	}
}
