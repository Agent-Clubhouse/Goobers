package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/coordination"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func coordinateWorkflowFixture(t *testing.T) (*coordinationKindExecutor, apiv1.Workflow, apiv1.InvocationEnvelope) {
	t.Helper()
	root := initDemo(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "coordination", "workflow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "gaggle: coordinator", "gaggle: example"))
	if err := os.WriteFile(filepath.Join(root, "config", "gaggles", "example", "workflows", "coordinate.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := coordinateWorkflow(instance.Layout{Root: root}, "example", "coordinate")
	if err != nil {
		t.Fatal(err)
	}
	var p coordination.Plan
	if err := readCoordinateJSON(filepath.Join("..", "..", "examples", "coordination", "plan.json"), &p); err != nil {
		t.Fatal(err)
	}
	p.Gaggle = "example"
	p.Parent.Repository = coordination.Repository{Provider: "github", Owner: "your-org", Name: "your-repo"}
	a := coordination.Authority{Name: p.Gaggle, ParentRepo: p.Parent.Repository, ApprovedPlans: map[string]string{}, ApprovedWorkflows: map[string]string{}}
	for _, child := range p.Children {
		a.Targets = append(a.Targets, coordination.Target{Repository: child.Repository, Approval: "reviewed-plan"})
	}
	a.ApprovedPlans[p.ID], _ = coordination.Digest(p)
	a.ApprovedWorkflows[w.Name], _ = coordination.Digest(w.Spec)
	if err := os.MkdirAll(filepath.Join(root, "coordination"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "coordination", "plan.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	reg, _ := journal.DefaultScrubber()
	e := &coordinationKindExecutor{input: deterministicExecutorInput{
		Config:       &instance.Config{Coordination: &coordination.Configuration{Gaggles: []coordination.Authority{a}}},
		InstanceRoot: root, SharedRegistry: reg,
		GaggleProject: apiv1.RepoRef{Provider: "github", Owner: "your-org", Name: "your-repo"},
	}}
	env := apiv1.InvocationEnvelope{RunID: "fake-manual-run", Gaggle: "example", WorkflowID: w.Name, TaskID: "fake-manual-run:" + w.Spec.Tasks[0].Name, Inputs: map[string]interface{}{}}
	env.RepoRef = e.input.GaggleProject
	for k, v := range w.Spec.Tasks[0].Inputs {
		env.Inputs[k] = v
	}
	return e, w, env
}

func TestCoordinateNativeManualDispatchWithoutCredentialInjection(t *testing.T) {
	e, w, env := coordinateWorkflowFixture(t)
	calls := 0
	e.reconcile = func(ctx context.Context, cfg *instance.Config, layout instance.Layout, a coordination.Authority, p coordination.Plan, evidence *coordination.Evidence, artifact string, reg terminalSecretRegistry) (coordination.Result, error) {
		calls++
		if os.Getenv("GOOBERS_CRED_COORDINATION_WRITE") != "" || len(env.Capabilities) != 0 {
			t.Fatal("runner coordination credential leaked to stage environment")
		}
		if err := p.Validate(a, true); err != nil {
			t.Fatal(err)
		}
		return coordination.Result{PlanID: p.ID, State: "waiting"}, nil
	}
	kinds := executor.NewKindRegistry()
	if err := kinds.Register(executor.KindShell, coordinationNoShell{}); err != nil {
		t.Fatal(err)
	}
	if err := kinds.Register(coordination.WorkflowKind, e); err != nil {
		t.Fatal(err)
	}
	dispatch, err := executor.NewTaskExecutor(kinds)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dispatch.Run(t.Context(), env, *w.Spec.Tasks[0].Run)
	if err != nil || calls != 1 || out.Status != apiv1.ResultSuccess || out.Outputs["state"] != "waiting" || out.Outputs["complete"] != false {
		t.Fatalf("native manual dispatch: %+v, %v, calls=%d", out, err, calls)
	}
	if !executor.StageRequiresInstanceRoot(w.Spec.Tasks[0].Run.Command, coordination.WorkflowKind) {
		t.Fatal("native coordination could be dispatched to a pod")
	}
	machine, err := workflow.Compile(workflow.Definition{DSLVersion: "2.0", Name: w.Name, Spec: w.Spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		Worktrees: &worktree.Manager{}, RunsDir: filepath.Join(t.TempDir(), "runs"), ScratchDir: t.TempDir(),
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return dispatch, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(t.Context(), runner.StartInput{
		RunID: strings.Repeat("a", 32), Machine: machine, Gaggle: env.Gaggle, RepoRef: e.input.GaggleProject,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil || result.Phase != journal.PhaseCompleted || calls != 2 {
		t.Fatalf("isolated manual runner: %+v %v calls=%d", result, err, calls)
	}
}

type coordinationNoShell struct{}

func (coordinationNoShell) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, fmt.Errorf("native coordination must never spawn a shell stage")
}

func TestCoordinateNativeRejectsUnauthorizedRequests(t *testing.T) {
	for _, mode := range []string{"wrong-gaggle", "wrong-project", "agentic", "nested", "capability", "unapproved-workflow", "unapproved-plan", "changed-workflow", "changed-plan", "changed-input", "extra-input", "pod"} {
		t.Run(mode, func(t *testing.T) {
			e, w, env := coordinateWorkflowFixture(t)
			e.reconcile = func(context.Context, *instance.Config, instance.Layout, coordination.Authority, coordination.Plan, *coordination.Evidence, string, terminalSecretRegistry) (coordination.Result, error) {
				t.Fatal("unauthorized request reached credential/provider operation")
				return coordination.Result{}, nil
			}
			switch mode {
			case "wrong-gaggle":
				env.Gaggle = "other"
			case "wrong-project":
				e.input.GaggleProject.Name = "other"
			case "agentic":
				env.Goober = "implementer"
			case "nested":
				env.ParentPlatformPolicy = &apiv1.PlatformPolicy{}
			case "capability":
				env.Capabilities = []string{"coordination:write"}
			case "unapproved-workflow":
				e.input.Config.Coordination.Gaggles[0].ApprovedWorkflows = nil
			case "unapproved-plan":
				e.input.Config.Coordination.Gaggles[0].ApprovedPlans = nil
			case "changed-workflow":
				path := filepath.Join(e.input.InstanceRoot, "config", "gaggles", "example", "workflows", "coordinate.yaml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "report its actual state", "report a different goal")), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-plan":
				path := filepath.Join(e.input.InstanceRoot, "coordination", "plan.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "Publish contract v2", "Publish unapproved scope")), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-input":
				env.Inputs["planFile"] = "other.json"
			case "extra-input":
				env.Inputs["token"] = "forbidden"
			case "pod":
				t.Setenv("KUBERNETES_SERVICE_HOST", "unsupported")
			}
			if _, err := e.Run(t.Context(), env, *w.Spec.Tasks[0].Run); err == nil {
				t.Fatal("unauthorized request accepted")
			}
		})
	}
}

func TestCoordinateManualWorkflowCompilesAndRejectsAgenticOrScheduled(t *testing.T) {
	_, w, _ := coordinateWorkflowFixture(t)
	for _, version := range []string{"2.0", "3.0"} {
		def := workflow.Definition{DSLVersion: version, Name: w.Name, Spec: w.Spec}
		if _, err := workflow.Compile(def); err != nil {
			t.Fatalf("manual %s: %v", version, err)
		}
		for _, mode := range []string{"schedule", "agentic", "dynamic", "credential"} {
			bad := w.DeepCopy()
			switch mode {
			case "schedule":
				bad.Spec.Triggers[0].Type = apiv1.TriggerSchedule
			case "agentic":
				bad.Spec.Tasks[0].Type = apiv1.TaskAgentic
			case "dynamic":
				bad.Spec.Tasks[0].InputsFrom = map[string]string{"planFile": "untrusted.plan"}
			case "credential":
				bad.Spec.Tasks[0].Capabilities = []string{"coordination:write"}
			}
			def.Spec = bad.Spec
			if _, err := workflow.Compile(def); err == nil {
				t.Fatalf("%s admitted %s coordination", version, mode)
			}
		}
	}
}

func TestCoordinatePrimaryOwnerAllowedButAmbiguousOwnerRejected(t *testing.T) {
	e, _, _ := coordinateWorkflowFixture(t)
	a := e.input.Config.Coordination.Gaggles[0]
	a.Targets = []coordination.Target{{Repository: a.ParentRepo, Approval: "reviewed-plan"}}
	owner := apiv1.Gaggle{}
	owner.Name, owner.Spec.Project = a.Name, e.input.GaggleProject
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{owner}}
	if err := coordinateGaggles(set, a); err != nil {
		t.Fatal(err)
	}
	other := *owner.DeepCopy()
	other.Name = "second-owner"
	set.Gaggles = append(set.Gaggles, other)
	if err := coordinateGaggles(set, a); err == nil {
		t.Fatal("ambiguous primary owner accepted")
	}
}

func TestCoordinateOfflineWorkflowApprovalDigest(t *testing.T) {
	e, w, _ := coordinateWorkflowFixture(t)
	layout := instance.Layout{Root: e.input.InstanceRoot}
	var p coordination.Plan
	if err := readCoordinateJSON(filepath.Join(layout.Root, "coordination", "plan.json"), &p); err != nil {
		t.Fatal(err)
	}
	out, err := coordinateCheckOutput(layout, w.Spec.Gaggle, w.Name, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["workflowDigest"] != e.input.Config.Coordination.Gaggles[0].ApprovedWorkflows[w.Name] {
		t.Fatal("workflow digest differs from runtime approval")
	}
	p.Summary = "unrelated plan"
	if _, err := coordinateCheckOutput(layout, w.Spec.Gaggle, w.Name, p, nil); err == nil {
		t.Fatal("check accepted a plan other than workflow planFile")
	}
}
