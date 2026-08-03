package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

type infraRetryNoWorkGoober struct {
	t       *testing.T
	commit  bool
	calls   int
	reviews int
}

func (g *infraRetryNoWorkGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	g.calls++
	if g.calls == 1 {
		if g.commit {
			if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("committed implementation\n"), 0o644); err != nil {
				return apiv1.ResultEnvelope{}, err
			}
			runGit(g.t, env.Workspace, "add", "-A")
			runGit(g.t, env.Workspace, "commit", "-m", "implement feature")
		}
		return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(errors.New("harness: no completion file written"))
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Summary: "already implemented"}, nil
}

func (g *infraRetryNoWorkGoober) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.reviews++
	assertCommittedImplementation(g.t, env.Workspace)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

type infraRetryPathDeterministic struct {
	t          *testing.T
	commitOn   map[string]bool
	stageCalls []string
}

func (d *infraRetryPathDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.t.Helper()
	stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
	d.stageCalls = append(d.stageCalls, stage)
	if stage != "prepare" {
		assertCommittedImplementation(d.t, env.Workspace)
	}
	if d.commitOn[stage] {
		if err := os.WriteFile(filepath.Join(env.Workspace, stage+".txt"), []byte(stage+"\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(d.t, env.Workspace, "add", "-A")
		runGit(d.t, env.Workspace, "commit", "-m", stage)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func assertCommittedImplementation(t *testing.T, workspace string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(workspace, "impl.txt"))
	if err != nil {
		t.Fatalf("read committed implementation: %v", err)
	}
	if string(got) != "committed implementation\n" {
		t.Fatalf("impl.txt = %q, want committed implementation", got)
	}
}

func infraRetryWorkflow(t *testing.T, prepare bool) *workflow.Machine {
	t.Helper()
	start := "implement"
	tasks := []apiv1.Task{
		{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement",
			Retry: &apiv1.RetryPolicy{MaxAttempts: 2}, Next: "review",
		},
		{
			Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "verify",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "push-branch",
		},
		{
			Name: "push-branch", Type: apiv1.TaskDeterministic, Goal: "push",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "open-pr",
		},
		{
			Name: "open-pr", Type: apiv1.TaskDeterministic, Goal: "open PR",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TerminalComplete,
		},
	}
	if prepare {
		start = "prepare"
		tasks = append(tasks, apiv1.Task{
			Name: "prepare", Type: apiv1.TaskDeterministic, Goal: "create earlier work",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "implement",
		})
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start: start, Tasks: tasks,
		Gates: []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic,
			Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{
				string(apiv1.VerdictPass):         "local-ci",
				string(apiv1.VerdictNeedsChanges): "implement",
				string(apiv1.VerdictFail):         workflow.TargetAbort,
			},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "infra-retry-work", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	return machine
}

func parallelInfraRetryWorkflow(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start: "fan",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement",
				Workspace: apiv1.WorkspaceScratch, Retry: &apiv1.RetryPolicy{MaxAttempts: 2}, Next: "review",
			},
			{
				Name: "inspect", Type: apiv1.TaskDeterministic, Goal: "inspect",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: workflow.TargetJoin,
			},
			{
				Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: workflow.TerminalComplete,
			},
		},
		Gates: []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic,
			Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch},
			Branches: map[string]string{
				string(apiv1.VerdictPass):         workflow.TargetJoin,
				string(apiv1.VerdictNeedsChanges): "implement",
				string(apiv1.VerdictFail):         workflow.TargetAbort,
			},
		}},
		Parallels: []apiv1.Parallel{{
			Name: "fan", FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrentBranches: 2, Join: "collate",
			Branches: []apiv1.Branch{
				{Name: "implementation", Start: "implement"},
				{Name: "inspection", Start: "inspect"},
			},
		}},
	}
	machine, err := workflow.Compile(
		workflow.Definition{Name: "parallel-infra-retry-work", Version: 1, DSLVersion: "2.0", Spec: spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile parallel workflow: %v", err)
	}
	return machine
}

func newInfraRetryRunner(t *testing.T, goober invoke.Goober, deterministic invoke.Deterministic) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Worktrees:    manager,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return r, runsDir
}

func TestRunnerPreservesCommittedWorkWhenInfrastructureRetryReturnsNoWork(t *testing.T) {
	goober := &infraRetryNoWorkGoober{t: t, commit: true}
	deterministic := &infraRetryPathDeterministic{t: t}
	r, runsDir := newInfraRetryRunner(t, goober, deterministic)

	res, err := r.Start(context.Background(), salvageStartInput("run-preserve-infra-work", infraRetryWorkflow(t, false)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if goober.calls != 2 || goober.reviews != 1 {
		t.Fatalf("implement calls = %d, reviews = %d; want 2 and 1", goober.calls, goober.reviews)
	}
	wantStages := []string{"local-ci", "push-branch", "open-pr"}
	if !reflect.DeepEqual(deterministic.stageCalls, wantStages) {
		t.Fatalf("downstream stages = %v, want %v", deterministic.stageCalls, wantStages)
	}

	events := readRunEvents(t, runsDir, "run-preserve-infra-work")
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "implement" && event.Attempt == 2 {
			if event.Status != string(apiv1.ResultSuccess) || event.Outputs["preservedCommittedWork"] != true {
				t.Fatalf("retry stage.finished = %+v, want preserved success", event)
			}
			return
		}
	}
	t.Fatal("missing implement retry stage.finished event")
}

func TestRunnerDoesNotPreserveEarlierStageCommitWhenFailedAttemptCreatedNone(t *testing.T) {
	goober := &infraRetryNoWorkGoober{t: t}
	deterministic := &infraRetryPathDeterministic{t: t, commitOn: map[string]bool{"prepare": true}}
	r, _ := newInfraRetryRunner(t, goober, deterministic)

	res, err := r.Start(context.Background(), salvageStartInput("run-earlier-work", infraRetryWorkflow(t, true)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if goober.reviews != 0 {
		t.Fatalf("review invoked %d times, want 0", goober.reviews)
	}
	if want := []string{"prepare"}; !reflect.DeepEqual(deterministic.stageCalls, want) {
		t.Fatalf("deterministic stages = %v, want %v", deterministic.stageCalls, want)
	}
}

func TestRunnerPreservesCommittedWorkAfterRestartBeforeInfrastructureRetry(t *testing.T) {
	const runID = "run-preserve-infra-work-after-restart"
	machine := infraRetryWorkflow(t, false)
	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: fixtureRepo,
		RunID:   runID + "-failed-attempt",
		BaseRef: "main",
		Branch:  providers.BranchName(machine.Def.Name, runID),
	})
	if err != nil {
		t.Fatalf("create failed-attempt worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "impl.txt"), []byte("committed implementation\n"), 0o644); err != nil {
		t.Fatalf("write implementation: %v", err)
	}
	runGit(t, wt.Path, "add", "-A")
	runGit(t, wt.Path, "commit", "-m", "implement feature")
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); err != nil {
		t.Fatalf("remove failed-attempt worktree: %v", err)
	}

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create interrupted run journal: %v", err)
	}
	jr.SetMachineState("implement")
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{
			Type: journal.EventError, Stage: "implement", Attempt: 1,
			Error: &journal.ErrorDetail{Code: "executor_error", Message: "harness: no completion file written"},
			Runner: map[string]any{
				retryFailureClassKey:  string(journal.AttemptInfra),
				infraCommittedWorkKey: true,
			},
		},
	} {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append interrupted run event: %v", err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close interrupted run journal: %v", err)
	}

	goober := &infraRetryNoWorkGoober{t: t, calls: 1}
	deterministic := &infraRetryPathDeterministic{t: t}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Worktrees:    manager,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new restarted runner: %v", err)
	}
	res, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if goober.calls != 2 || goober.reviews != 1 {
		t.Fatalf("post-restart implement calls = %d, reviews = %d; want 1 and 1", goober.calls-1, goober.reviews)
	}
	wantStages := []string{"local-ci", "push-branch", "open-pr"}
	if !reflect.DeepEqual(deterministic.stageCalls, wantStages) {
		t.Fatalf("downstream stages = %v, want %v", deterministic.stageCalls, wantStages)
	}
}

func TestRunnerPreservesParallelBranchCommittedWorkAfterRestartBeforeInfrastructureRetry(t *testing.T) {
	const runID = "run-preserve-parallel-infra-work-after-restart"
	machine := parallelInfraRetryWorkflow(t)
	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: fixtureRepo,
		RunID:   runID + "-failed-attempt",
		BaseRef: "main",
		Branch:  providers.BranchName(machine.Def.Name, runID),
	})
	if err != nil {
		t.Fatalf("create failed-attempt worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "impl.txt"), []byte("committed implementation\n"), 0o644); err != nil {
		t.Fatalf("write implementation: %v", err)
	}
	runGit(t, wt.Path, "add", "-A")
	runGit(t, wt.Path, "commit", "-m", "implement parallel feature")
	runGit(t, wt.Path, "push", fixtureRepo, providers.BranchName(machine.Def.Name, runID))
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); err != nil {
		t.Fatalf("remove failed-attempt worktree: %v", err)
	}

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create interrupted run journal: %v", err)
	}
	jr.SetMachineState("fan")
	for _, event := range []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{
			Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1,
			BranchName: "implementation", Stage: "implement",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Branch: 1, Attempt: 1},
		{
			Type: journal.EventError, Stage: "implement", Branch: 1, Attempt: 1,
			Error: &journal.ErrorDetail{Code: "executor_error", Message: "harness: no completion file written"},
			Runner: map[string]any{
				retryFailureClassKey:  string(journal.AttemptInfra),
				infraCommittedWorkKey: true,
			},
		},
	} {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append interrupted parallel event: %v", err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close interrupted run journal: %v", err)
	}

	goober := &infraRetryNoWorkGoober{t: t, calls: 1}
	deterministic := &infraRetryPathDeterministic{t: t}
	r, err := New(Config{
		PinnedWorkspace: true,
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Worktrees:    manager,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new restarted runner: %v", err)
	}
	res, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if goober.calls != 2 || goober.reviews != 1 {
		t.Fatalf("post-restart implement calls = %d, reviews = %d; want 1 and 1", goober.calls-1, goober.reviews)
	}
	if want := []string{"inspect", "collate"}; !reflect.DeepEqual(deterministic.stageCalls, want) {
		t.Fatalf("deterministic stages = %v, want %v", deterministic.stageCalls, want)
	}
}
