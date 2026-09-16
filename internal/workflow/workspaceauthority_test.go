package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestWorkspaceAuthorityDiagnostics(t *testing.T) {
	fixture := func() Definition {
		return Definition{Name: "sandbox", Spec: apiv1.WorkflowSpec{Start: "establish", Tasks: []apiv1.Task{
			{Name: "establish", Type: apiv1.TaskDeterministic, Inputs: map[string]string{"kind": "workspace-branch-establish"},
				Capabilities: []string{"repo:push"}, Run: &apiv1.DeterministicRun{Workspace: apiv1.WorkspaceScratch}, Next: "author"},
			{Name: "author", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Workspace: apiv1.WorkspaceRepo}, Next: "publish"},
			{Name: "publish", Type: apiv1.TaskDeterministic, Inputs: map[string]string{"kind": "workspace-branch-publish"},
				Capabilities: []string{"repo:push"}, Run: &apiv1.DeterministicRun{Workspace: apiv1.WorkspaceRepo}},
		}}}
	}
	if problems := CheckWorkspaceAuthority(fixture()); len(problems) != 0 {
		t.Fatal(problems)
	}
	for _, name := range []string{"missing-establishment", "agent-producer", "continue-error", "readonly", "scalar-authority", "sync-base", "delta", "gate-bypass", "parallel-author", "dynamic-kind"} {
		t.Run(name, func(t *testing.T) {
			def := fixture()
			switch name {
			case "missing-establishment":
				def.Spec.Start = "author"
			case "agent-producer":
				def.Spec.Tasks[0].Type = apiv1.TaskAgentic
			case "continue-error":
				def.Spec.Tasks[0].ContinueOnError = true
			case "readonly":
				def.Spec.Tasks[1].Run.Workspace = apiv1.WorkspaceRepoReadOnly
			case "scalar-authority":
				def.Spec.Tasks[1].Inputs = map[string]string{"workspaceRevision": "untrusted"}
			case "sync-base":
				def.Spec.Tasks[0].Run.SyncBase = true
			case "delta":
				def.Spec.Tasks[1].Run.Workspace = apiv1.WorkspaceRepoReadOnly
				def.Spec.Tasks[1].InputsFrom = map[string]string{"workspaceDelta": "select.delta"}
			case "gate-bypass":
				def.Spec.Start = "choose"
				def.Spec.Gates = []apiv1.Gate{{Name: "choose", Branches: map[string]string{"yes": "establish", "no": "author"}}}
			case "parallel-author":
				def.Spec.Tasks[0].Next = "fanout"
				def.Spec.Parallels = []apiv1.Parallel{{Name: "fanout", Join: "publish", Branches: []apiv1.Branch{{Start: "author"}}}}
			case "dynamic-kind":
				def.Spec.Tasks[0].InputsFrom = map[string]string{"kind": "selector.kind"}
			}
			if problems := CheckWorkspaceAuthority(def); len(problems) == 0 || !strings.Contains(strings.Join(problems, " "), "workspace_") {
				t.Fatalf("missing diagnostic: %v", problems)
			}
		})
	}
	legacy := fixture()
	legacy.Spec.Tasks = legacy.Spec.Tasks[1:2]
	legacy.Spec.Start = "author"
	if problems := CheckWorkspaceAuthority(legacy); len(problems) != 0 {
		t.Fatalf("legacy writable behavior changed: %v", problems)
	}
}
