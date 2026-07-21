package engine

import (
	"context"
	"os"
	"os/exec"
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

func loadCustomStageWorkflow(t *testing.T) apiv1.WorkflowSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(customStageConfigRoot, "workflows", "todo-check.yaml"))
	if err != nil {
		t.Fatalf("read todo-check.yaml: %v", err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("unmarshal todo-check.yaml: %v", err)
	}
	return workflow.Spec
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
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
	return workspace
}

func TestCustomStageExampleDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("example task intentionally exercises a POSIX shell script")
	}
	spec := loadCustomStageWorkflow(t)
	machine, err := wf.Compile(wf.Definition{Name: "todo-check", Version: 1, Spec: spec})
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
