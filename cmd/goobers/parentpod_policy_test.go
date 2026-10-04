package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
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

func TestContainedParentSelectionKeepsDurableRunnerWithWorkerStages(t *testing.T) {
	f := containedParentFixture(t)
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	id := localscheduler.WorkflowIdentity{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow}
	selections, err := engineSelections(f.cfg, f.applied, map[localscheduler.WorkflowIdentity]*workflow.Machine{id: machine})
	if err != nil {
		t.Fatal(err)
	}
	selection := selections[id]
	if !selection.ContainedParent || selection.UseEngine {
		t.Fatal("parent did not retain local durable coordinator", selection)
	}
	if caps := selection.schedulerSelfCapabilities([]string{"local-only"}); len(caps) != 0 {
		t.Fatal(caps)
	}
	pins, err := containedParentPlacements(f.cfg, f.applied, machine)
	if err != nil || len(pins) != 1 || pins[0].Self {
		t.Fatal(pins, err)
	}
}

func TestContainedParentUnsupportedShapesRefuse(t *testing.T) {
	for _, mode := range []string{"gate", "parallel", "ordinary-task", "scratch", "broader-credential", "copilot", "no-worker", "self"} {
		t.Run(mode, func(t *testing.T) {
			f := containedParentFixture(t)
			_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: f.parent.Gaggle, Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "gate":
				machine.Def.Spec.Gates = []apiv1.Gate{{Name: "gate"}}
			case "parallel":
				machine.Def.Spec.Parallels = []apiv1.Parallel{{Name: "fork"}}
			case "ordinary-task":
				machine.Def.Spec.Tasks[0].ChildWorkflows = nil
			case "scratch":
				machine.Def.Spec.Tasks[0].Workspace = apiv1.WorkspaceScratch
			case "broader-credential":
				machine.Def.Spec.Tasks[0].Capabilities = []string{"repo:push"}
			case "copilot":
				for i := range f.applied.Goobers {
					f.applied.Goobers[i].Spec.Harness = apiv1.HarnessCopilot
				}
			case "no-worker":
				f.cfg = &instance.Config{API: f.cfg.API}
			case "self":
				machine.Def.Spec.Tasks[0].RunsOn = nil
			}
			if _, err = containedParentPlacements(f.cfg, f.applied, machine); err == nil {
				t.Fatal("unsupported parent admitted")
			}
		})
	}
}

type parentRouteFake struct{ calls int }

func (p *parentRouteFake) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	p.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (p *parentRouteFake) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, nil
}

func TestContainedParentMissingBackendNeverFallsBackToHost(t *testing.T) {
	base := &parentRouteFake{}
	cfg := withContainedParentExecutor(runner.Config{NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) { return base, nil }}, t.TempDir(), &instance.Config{}, nil)
	executor, err := cfg.NewAgentic("coder", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executor.Invoke(t.Context(), apiv1.InvocationEnvelope{}); err != nil || base.calls != 1 {
		t.Fatal(err, base.calls)
	}
	if _, err = executor.Invoke(t.Context(), apiv1.InvocationEnvelope{ChildWorkflowOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "occurrence", AttemptID: "attempt"}}); err == nil || base.calls != 1 {
		t.Fatal("parent used local fallback", err, base.calls)
	}
}
