package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	wf "github.com/goobers/goobers/internal/workflow"
)

const customStageConfigRoot = "../../config-examples/gaggles/acme-web"

type customStageRecorder struct {
	mu       sync.Mutex
	recorded map[string][]byte
}

func (r *customStageRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := append([]byte(nil), data...)
	r.recorded[name] = copied
	return journal.Ref{Path: name, Digest: journal.Digest(copied), Size: int64(len(copied))}, nil
}

type customStageRegistrar struct{}

func (customStageRegistrar) Register([]byte) {}

func loadCustomStageWorkflow(t *testing.T, name string) apiv1.Workflow {
	t.Helper()
	filename := name + ".yaml"
	raw, err := os.ReadFile(filepath.Join(customStageConfigRoot, "workflows", filename))
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("unmarshal %s: %v", filename, err)
	}
	return workflow
}

func customStageWorkspace(t *testing.T, fixture string) string {
	t.Helper()
	workspace := t.TempDir()
	script, err := os.ReadFile(filepath.Join(customStageConfigRoot, "scripts", "check-todos.sh"))
	if err != nil {
		t.Fatalf("read check-todos.sh: %v", err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "scripts"), 0o755); err != nil {
		t.Fatalf("create scripts directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "scripts", "check-todos.sh"), script, 0o755); err != nil {
		t.Fatalf("write check-todos.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "fixture.go"), []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", workspace},
		{"-C", workspace, "add", "fixture.go"},
	} {
		command := testgit.Command(args...)
		command.Env = append(command.Env,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=core.autocrlf",
			"GIT_CONFIG_VALUE_0=false",
			"GIT_CONFIG_KEY_1=core.safecrlf",
			"GIT_CONFIG_VALUE_1=false",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
	return workspace
}

func newCustomStageShell(t *testing.T) (*executor.ShellExecutor, *customStageRecorder) {
	t.Helper()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("create credential resolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, nil, customStageRegistrar{})
	if err != nil {
		t.Fatalf("create credential injector: %v", err)
	}
	recorder := &customStageRecorder{recorded: map[string][]byte{}}
	shell, err := executor.NewShellExecutor(injector, recorder)
	if err != nil {
		t.Fatalf("create shell executor: %v", err)
	}
	return shell, recorder
}

func TestCustomStageExampleDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("example task intentionally exercises a POSIX shell script")
	}
	workflow := loadCustomStageWorkflow(t, "todo-check")
	spec := workflow.Spec
	machine, err := wf.Compile(
		wf.Definition{Name: "todo-check", Version: 1, DSLVersion: workflow.DSLVersion, Spec: spec},
		wf.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile todo-check workflow: %v", err)
	}
	checkTask, ok := machine.Task("check-todos")
	if !ok || checkTask.Run == nil {
		t.Fatal("compiled workflow is missing the check-todos deterministic task")
	}
	todosGate, ok := machine.Gate("todos-found")
	if !ok || todosGate.Automated == nil {
		t.Fatal("compiled workflow is missing the todos-found automated gate")
	}

	tests := []struct {
		name        string
		fixture     string
		wantCount   float64
		wantOutcome string
		wantTarget  string
	}{
		{
			name:        "todos found",
			fixture:     "package fixture\n\n// TODO: first\n// TODO: second\n",
			wantCount:   2,
			wantOutcome: gate.OutcomePass,
			wantTarget:  "report-todos",
		},
		{
			name:        "clean",
			fixture:     "package fixture\n",
			wantCount:   0,
			wantOutcome: gate.OutcomeFail,
			wantTarget:  "report-clean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shell, recorder := newCustomStageShell(t)

			inputs := make(map[string]interface{}, len(checkTask.Inputs))
			for key, value := range checkTask.Inputs {
				inputs[key] = value
			}
			const taskID = "run-todo-check:check-todos"
			result, err := shell.Run(context.Background(), apiv1.InvocationEnvelope{
				TaskID:       taskID,
				Workspace:    customStageWorkspace(t, tt.fixture),
				Inputs:       inputs,
				Capabilities: checkTask.Capabilities,
			}, *checkTask.Run)
			if err != nil {
				t.Fatalf("run check-todos: %v", err)
			}
			if result.Status != apiv1.ResultSuccess {
				t.Fatalf("check-todos status = %q, want success", result.Status)
			}
			if got := result.Outputs["todoCount"]; got != tt.wantCount {
				t.Fatalf("todoCount = %#v, want %v", got, tt.wantCount)
			}
			if len(result.Artifacts) != 3 {
				t.Fatalf("artifacts = %d, want stdout, stderr, and result-file pointers", len(result.Artifacts))
			}
			stdout := string(recorder.recorded[taskID+"/stdout.log"])
			if got := strings.Count(stdout, "\n"); got != int(tt.wantCount) {
				t.Fatalf("stdout listing has %d lines, want %v: %q", got, tt.wantCount, stdout)
			}

			gateInputs := map[string]interface{}{gate.InputKeyStatus: string(result.Status)}
			for key, value := range result.Outputs {
				gateInputs[key] = value
			}
			outcome, err := gate.NewAutomatedEvaluator().Evaluate(
				context.Background(),
				*todosGate.Automated,
				apiv1.InvocationEnvelope{Inputs: gateInputs},
			)
			if err != nil {
				t.Fatalf("evaluate todos-found: %v", err)
			}
			if outcome != tt.wantOutcome {
				t.Fatalf("gate outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			target, ok := wf.BranchTarget(todosGate, outcome)
			if !ok || target != tt.wantTarget {
				t.Fatalf("gate target = %q, %v; want %q, true", target, ok, tt.wantTarget)
			}
			if _, ok := machine.Task(target); !ok {
				t.Fatalf("gate target %q is not a compiled task", target)
			}
		})
	}
}

func TestInlineCustomStageExampleDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shipped example declares os=linux and uses POSIX script syntax")
	}
	workflow := loadCustomStageWorkflow(t, "inline-policy-check")
	machine, err := wf.Compile(
		wf.Definition{
			Name:       workflow.Name,
			Version:    1,
			DSLVersion: workflow.DSLVersion,
			Spec:       workflow.Spec,
		},
		wf.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile inline-policy-check workflow: %v", err)
	}
	checkTask, ok := machine.Task("check-label")
	if !ok || checkTask.Run == nil {
		t.Fatal("compiled workflow is missing the check-label inline task")
	}
	policyGate, ok := machine.Gate("label-policy")
	if !ok || policyGate.Automated == nil {
		t.Fatal("compiled workflow is missing the label-policy automated gate")
	}

	tests := []struct {
		name        string
		labels      string
		wantAllowed bool
		wantOutcome string
		wantTarget  string
		wantReport  string
	}{
		{
			name:        "required label present",
			labels:      "ready,security-reviewed",
			wantAllowed: true,
			wantOutcome: gate.OutcomePass,
			wantTarget:  "report-allowed",
			wantReport:  "Required review label is present.",
		},
		{
			name:        "required label missing",
			labels:      "ready",
			wantAllowed: false,
			wantOutcome: gate.OutcomeFail,
			wantTarget:  "report-blocked",
			wantReport:  "Required review label is missing.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shell, recorder := newCustomStageShell(t)
			inputs := make(map[string]interface{}, len(checkTask.Inputs))
			for key, value := range checkTask.Inputs {
				inputs[key] = value
			}
			inputs["labels"] = tt.labels

			const checkTaskID = "run-inline-policy-check:check-label"
			result, err := shell.Run(context.Background(), apiv1.InvocationEnvelope{
				TaskID:       checkTaskID,
				Workspace:    t.TempDir(),
				Inputs:       inputs,
				Capabilities: checkTask.Capabilities,
			}, *checkTask.Run)
			if err != nil {
				t.Fatalf("run check-label: %v", err)
			}
			if result.Status != apiv1.ResultSuccess {
				t.Fatalf("check-label status = %q, want success", result.Status)
			}
			if got := result.Outputs["allowed"]; got != tt.wantAllowed {
				t.Fatalf("allowed = %#v, want %v", got, tt.wantAllowed)
			}

			gateInputs := map[string]interface{}{gate.InputKeyStatus: string(result.Status)}
			for key, value := range result.Outputs {
				gateInputs[key] = value
			}
			outcome, err := gate.NewAutomatedEvaluator().Evaluate(
				context.Background(),
				*policyGate.Automated,
				apiv1.InvocationEnvelope{Inputs: gateInputs},
			)
			if err != nil {
				t.Fatalf("evaluate label-policy: %v", err)
			}
			if outcome != tt.wantOutcome {
				t.Fatalf("gate outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			target, ok := wf.BranchTarget(policyGate, outcome)
			if !ok || target != tt.wantTarget {
				t.Fatalf("gate target = %q, %v; want %q, true", target, ok, tt.wantTarget)
			}

			reportTask, ok := machine.Task(target)
			if !ok || reportTask.Run == nil {
				t.Fatalf("gate target %q is not a compiled deterministic task", target)
			}
			reportTaskID := "run-inline-policy-check:" + target
			report, err := shell.Run(context.Background(), apiv1.InvocationEnvelope{
				TaskID:    reportTaskID,
				Workspace: t.TempDir(),
			}, *reportTask.Run)
			if err != nil {
				t.Fatalf("run %s: %v", target, err)
			}
			if report.Status != apiv1.ResultSuccess {
				t.Fatalf("%s status = %q, want success", target, report.Status)
			}
			if got := strings.TrimSpace(string(recorder.recorded[reportTaskID+"/stdout.log"])); got != tt.wantReport {
				t.Fatalf("%s stdout = %q, want %q", target, got, tt.wantReport)
			}
		})
	}
}
