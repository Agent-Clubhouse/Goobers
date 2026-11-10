package runner

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type revisitMergeExecutor struct {
	mu    sync.Mutex
	calls map[string]int
	joins []string
}

func (e *revisitMergeExecutor) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
	e.calls[stage]++
	value := fmt.Sprintf("%s-%d", stage, e.calls[stage])
	grade := apiv1.IntegrityTrusted
	if stage == "alpha" && e.calls[stage] == 2 {
		grade = apiv1.IntegrityUnapproved
	}
	if stage == "join" {
		value = fmt.Sprintf("%v/%v", env.Inputs["alpha"], env.Inputs["beta"])
		e.joins = append(e.joins, value)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: grade, Outputs: map[string]any{"value": value}}, nil
}

func TestRunnerParallelRevisitMergesOnlyOwnedOutputs(t *testing.T) {
	for _, width := range []int32{1, 2} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			exec := &revisitMergeExecutor{calls: map[string]int{}}
			r, dir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return exec, nil }, &failThenPassAutomated{})
			r.cfg.ScratchDir = t.TempDir()
			id := fmt.Sprintf("parallel-revisit-%d", width)
			result, err := r.Start(context.Background(), StartInput{RunID: id, Machine: revisitMergeMachine(t, width), Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
			if err != nil || result.Phase != journal.PhaseCompleted {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			want := []string{"alpha-1/beta-1", "alpha-2/beta-2"}
			if !reflect.DeepEqual(exec.joins, want) {
				t.Errorf("join invocations=%v want=%v", exec.joins, want)
			}
			var alpha, joins []string
			var grades []apiv1.Integrity
			for _, event := range readRunnerEvents(t, dir, id) {
				if event.Type != journal.EventStageFinished {
					continue
				}
				if event.Stage == "alpha" {
					alpha = append(alpha, fmt.Sprint(event.Outputs["value"]))
				}
				if event.Stage == "join" {
					joins = append(joins, fmt.Sprint(event.Outputs["value"]))
					grades = append(grades, event.Integrity)
				}
			}
			if !reflect.DeepEqual(alpha, []string{"alpha-1", "alpha-2"}) {
				t.Errorf("producer journal=%v", alpha)
			}
			if !reflect.DeepEqual(joins, want) {
				t.Errorf("join journal=%v want=%v", joins, want)
			}
			if !reflect.DeepEqual(grades, []apiv1.Integrity{apiv1.IntegrityTrusted, apiv1.IntegrityUnapproved}) {
				t.Errorf("join grades=%v: stale seed must not launder fresh unapproved output", grades)
			}
		})
	}
}

func revisitMergeMachine(t *testing.T, width int32) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: next}
	}
	seed, alpha, beta, join := task("seed", "parallel"), task("alpha", workflow.TargetJoin), task("beta", workflow.TargetJoin), task("join", "again")
	alpha.ExpectedOutputs, beta.ExpectedOutputs = []string{"value"}, []string{"value"}
	join.InputsFrom = map[string]string{"alpha": "parallel.alpha.value", "beta": "parallel.beta.value"}
	machine, err := workflow.Compile(workflow.Definition{Name: "revisit", Version: 1, DSLVersion: "3.0", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "seed", Tasks: []apiv1.Task{seed, alpha, beta, join},
		Gates:     []apiv1.Gate{{Name: "again", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{"pass": workflow.TerminalComplete, "fail": "parallel"}}},
		Parallels: []apiv1.Parallel{{Name: "parallel", Branches: []apiv1.Branch{{Name: "alpha", Start: "alpha"}, {Name: "beta", Start: "beta"}}, Join: "join", FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrentBranches: width}},
	}}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}
