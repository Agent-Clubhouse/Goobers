package main

import (
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
)

func containedParentFixture(t *testing.T, change ...func(string) string) pinnedChildFixture {
	t.Helper()
	return newPinnedChildFixture(t, func(root string) {
		path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
		parent := strings.Replace(childValidationParent, "      goal:", "      workspace: repo\n      runsOn: {os: linux, capabilities: [isolated-parent]}\n      goal:", 1)
		for _, apply := range change {
			parent = apply(parent)
		}
		writeFileContent(t, path, parent)
		path = filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
		writeFileContent(t, path, strings.Replace(readFileContent(t, path), "harness: copilot", "harness: claude-code", 1))
		path = filepath.Join(root, "instance.yaml")
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(readFileContent(t, path)), &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "runner")
		doc["schemaVersion"] = 2
		grants, _ := doc["credentials"].([]any)
		doc["credentials"] = append(grants, map[string]any{"capability": "agent:model", "harness": "claude-code", "token": map[string]any{"env": "PARENT_TEST_MODEL_TOKEN"}})
		doc["engine"] = map[string]any{"hostPort": "temporal:7233"}
		doc["api"] = map[string]any{"podTokenKeyFile": filepath.Join(root, "pod-key")}
		doc["runners"] = []any{map[string]any{"name": "self", "host": "self"}, map[string]any{"name": "isolated", "host": "ghcr.io/example/parent:1", "provides": map[string]any{"os": "linux", "harnesses": []string{"claude-code", "claude"}, "capabilities": []string{"isolated-parent"}}}}
		data, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		writeFileContent(t, path, string(data))
	})
}

func TestContainedParentPlacementScopesOnlyOptedInStages(t *testing.T) {
	f := containedParentFixture(t)
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary tasks, gates and delegated provider permissions must not turn
	// this into the prototype's all-agentic/all-opted-in workflow restriction.
	machine.Def.Spec.Tasks[0].Capabilities = append(machine.Def.Spec.Tasks[0].Capabilities, "repo:push", "provider:pr:write")
	machine.Def.Spec.Tasks = append(machine.Def.Spec.Tasks, apiv1.Task{Name: "prepare", Type: apiv1.TaskDeterministic})
	machine.Def.Spec.Gates = append(machine.Def.Spec.Gates, apiv1.Gate{Name: "human-review"})
	pins, err := containedParentPlacements(f.cfg, f.applied, machine)
	if err != nil || len(pins) != 1 || pins[0].Stage != "plan" || pins[0].Self {
		t.Fatal("mixed parent workflow refused", pins, err)
	}
}

func TestContainedParentPlacementRequiresIsolatedSelectedStage(t *testing.T) {
	for _, mode := range []string{"scratch", "copilot", "no-worker", "self", "not-opted-in"} {
		t.Run(mode, func(t *testing.T) {
			f := containedParentFixture(t)
			_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "scratch":
				machine.Def.Spec.Tasks[0].Workspace = apiv1.WorkspaceScratch
			case "copilot":
				for i := range f.applied.Goobers {
					f.applied.Goobers[i].Spec.Harness = apiv1.HarnessCopilot
				}
			case "no-worker":
				f.cfg = &instance.Config{API: f.cfg.API}
			case "self":
				machine.Def.Spec.Tasks[0].RunsOn = nil
			case "not-opted-in":
				machine.Def.Spec.Tasks[0].ChildWorkflows = nil
			}
			if _, err := containedParentPlacements(f.cfg, f.applied, machine); err == nil {
				t.Fatal("unsupported placement admitted")
			}
		})
	}
}
