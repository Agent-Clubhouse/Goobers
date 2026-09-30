package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// A live qualification seed asked for a PR referencing its issue. The model
// committed the change but failed with PROVIDER_ACTION_REQUIRED for the PR that
// the workflow's next deterministic stages already owned (#6030).
func TestReferenceImplementationDefersOwnedProviderActions(t *testing.T) {
	for _, name := range []string{"implementation", "implementation-recovery", "implementation-pre-review-experiment"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var definition apiv1.Workflow
			if err := yaml.Unmarshal(raw, &definition); err != nil {
				t.Fatal(err)
			}
			tasks := make(map[string]apiv1.Task)
			for _, task := range definition.Spec.Tasks {
				tasks[task.Name] = task
			}
			goal := strings.Join(strings.Fields(tasks["implement"].Goal), " ")
			for _, required := range []string{
				"push-branch publishes the run branch",
				"open-pr creates or updates the PR and links the claimed issue",
				"close-out posts the standard completion comment",
				"Leave those actions pending for their named stages",
				"do not attempt them or claim they already happened",
				"pending state is not an implement-stage failure",
				"return success once your code deliverable is complete",
				"not an arbitrary requested comment, report, release, merge, or external update",
				"only when attached context proves",
				"PROVIDER_ACTION_REQUIRED",
			} {
				if !strings.Contains(goal, required) {
					t.Errorf("implement goal omits scoped provider handoff %q", required)
				}
			}
			if !slices.Equal(tasks["implement"].Capabilities, []string{"repo:push", "agent:model"}) {
				t.Errorf("implement capabilities widened: %v", tasks["implement"].Capabilities)
			}
			assertReferenceProviderHandoffTasks(t, tasks)
		})
	}
}

func assertReferenceProviderHandoffTasks(t *testing.T, tasks map[string]apiv1.Task) {
	t.Helper()
	for _, handoff := range []struct {
		name       string
		command    string
		capability string
	}{
		{"push-branch", "push-branch", "repo:push"},
		{"open-pr", "open-pr", "provider:pr:write"},
		{"close-out", "issue-close-out", "github:issues:write"},
	} {
		task, ok := tasks[handoff.name]
		if !ok || task.Type != apiv1.TaskDeterministic || task.Run == nil ||
			!slices.Equal(task.Run.Command, []string{"goobers", handoff.command}) ||
			!slices.Contains(task.Capabilities, handoff.capability) {
			t.Errorf("promised handoff %q lacks its deterministic command/capability", handoff.name)
		}
	}
	if tasks["close-out"].Inputs["status"] != "in-review" {
		t.Error("close-out must leave landing to separate merge review")
	}
}

func TestReferenceImplementerDistinguishesDeferredAndUnhandledActions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "goobers", "implementer", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	instructions := strings.Join(strings.Fields(string(raw)), " ")
	for _, required := range []string{
		"Workflow-owned follow-up actions are pending, not failed implement work.",
		"`push-branch` publishes the branch",
		"`open-pr` creates or updates the PR and links the claimed issue",
		"`close-out` posts the standard completion comment",
		"Do not emit `PROVIDER_ACTION_REQUIRED` merely because one of these named actions is still pending",
		"never claim it already happened",
		"Do not infer that an arbitrary requested report, comment, release, merge, or external update is covered",
		"attached context explicitly proves",
		"`error.code: PROVIDER_ACTION_REQUIRED`",
		"never assume or silently claim",
	} {
		if !strings.Contains(instructions, required) {
			t.Errorf("implementer instructions omit scoped provider rule %q", required)
		}
	}
}
