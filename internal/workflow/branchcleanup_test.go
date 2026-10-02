package workflow

import (
	"path/filepath"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestShippedBranchCleanupWorkflowIsScheduledAndDryRunByDefault pins the
// reference branch-cleanup workflow (#2509): a bounded daily schedule that
// runs the existing reconcile-branches command, reporting candidates without
// deleting anything until an operator opts in.
func TestShippedBranchCleanupWorkflowIsScheduledAndDryRunByDefault(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "branch-cleanup.yaml")
	var w apiv1.Workflow
	readResweepFixture(t, path, &w)
	def := Definition{Name: w.Name, Version: 1, Spec: w.Spec}
	m, err := compileAcknowledged(def)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := CheckWarnings(def); len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(w.Spec.Triggers) != 1 || w.Spec.Triggers[0].Type != apiv1.TriggerSchedule || w.Spec.Triggers[0].Schedule == "" {
		t.Fatalf("missing schedule trigger: %+v", w.Spec.Triggers)
	}
	if w.Spec.Readiness.MaxConcurrentRuns != 1 || w.Spec.Readiness.MaxRunsPerHour != 1 || w.Spec.Readiness.MaxRunsPerDay != 1 {
		t.Fatalf("missing once-a-day readiness bounds: %+v", w.Spec.Readiness)
	}
	task, ok := m.Task(w.Spec.Start)
	if !ok || task.Run == nil || !slices.Equal(task.Run.Command, []string{"goobers", "reconcile-branches"}) {
		t.Fatalf("start task is not a plain reconcile-branches sweep: %+v", task)
	}
	if task.Inputs["deleteBranches"] != "false" {
		t.Fatalf("deleteBranches = %q, want an explicit dry-run default", task.Inputs["deleteBranches"])
	}
	if len(task.PolicyActions) != 0 {
		t.Fatalf("dry-run sweep declares policy actions %v; delete-branch belongs only to an opted-in sweep", task.PolicyActions)
	}
	if !slices.Equal(task.Capabilities, []string{"github:branch:delete"}) {
		t.Fatalf("capabilities = %v, want only github:branch:delete", task.Capabilities)
	}
	if task.Next != "" {
		t.Fatalf("branch cleanup should be a single-stage workflow, next = %q", task.Next)
	}
}
