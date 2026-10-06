package runner

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

type gatePreparationRetryReviewer struct {
	calls atomic.Int32
}

type gatePreparationStartResult struct {
	result Result
	err    error
}

type blockingBranchReviewer struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*gatePreparationRetryReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *gatePreparationRetryReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.calls.Add(1)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func (*blockingBranchReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *blockingBranchReviewer) Review(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	case <-ctx.Done():
		return apiv1.Verdict{}, ctx.Err()
	}
}

func TestGatePreparationRetriesBranchOccupancyWithoutConsumingGateBudget(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "review",
		Gates: []apiv1.Gate{{
			Name:      "review",
			Evaluator: apiv1.EvaluatorAgentic,
			Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Retry: &apiv1.RetryPolicy{MaxAttempts: 1}},
			Branches:  map[string]string{"pass": workflow.TerminalComplete, "needs-changes": "review", "fail": workflow.TargetAbort},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "gate-prep-retry", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	runsDir, repo, worktrees := newTestRunnerEnv(t)
	const runID = "gate-prep-retry-run"
	branch := providers.BranchName(machine.Def.Name, runID)
	occupant, err := worktrees.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: "foreign-review", OwnerRunID: "foreign-run", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupant.Remove(context.Background(), worktree.RemoveOptions{}) })

	reviewer := &gatePreparationRetryReviewer{}
	runner, err := New(Config{
		RunsDir:      runsDir,
		Worktrees:    worktrees,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		Automated:    gate.NewAutomatedEvaluator(),
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.gatePrepRetryFloor = 100 * time.Millisecond

	done := make(chan gatePreparationStartResult, 1)
	go func() {
		result, err := runner.Start(context.Background(), StartInput{
			RunID:   runID,
			Machine: machine,
			Gaggle:  spec.Gaggle,
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- gatePreparationStartResult{result: result, err: err}
	}()

	waitForPreparationRetryEvent(t, runsDir, runID, "review", done)
	if err := occupant.Remove(context.Background(), worktree.RemoveOptions{}); err != nil {
		t.Fatalf("release branch occupant: %v", err)
	}
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("Start: %v", outcome.err)
	}
	if outcome.result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", outcome.result.Phase)
	}
	if got := reviewer.calls.Load(); got != 1 {
		t.Fatalf("reviewer calls = %d, want one successful evaluation after preparation retry", got)
	}
}

func TestImplementationCloseOutWaitsForMergeReviewOnSameBranch(t *testing.T) {
	const (
		mergeRunID          = "merge-review-run"
		implementationRunID = "implementation-run"
	)
	mergeMachine := compileBranchCollisionMachine(t, "merge-review", true)
	implementationMachine := compileBranchCollisionMachine(t, "implementation", false)

	instanceRoot := t.TempDir()
	runsDir := filepath.Join(instanceRoot, "runs")
	repo := newRebindFixtureRepo(t)
	worktrees, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &blockingBranchReviewer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	mergeRunner, err := New(Config{
		RunsDir:      runsDir,
		Worktrees:    worktrees,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				mergeRunID + ":select-pr": {
					status:  apiv1.ResultSuccess,
					outputs: map[string]interface{}{WorkspaceBranchOutput: rebindBranch},
				},
			}}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	implementationRunner, err := New(Config{
		RunsDir:      runsDir,
		Worktrees:    worktrees,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				implementationRunID + ":select-pr": {
					status:  apiv1.ResultSuccess,
					outputs: map[string]interface{}{WorkspaceBranchOutput: rebindBranch},
				},
				implementationRunID + ":close-out": {status: apiv1.ResultSuccess},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	implementationRunner.gatePrepRetryFloor = 100 * time.Millisecond

	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	mergeDone := startRunner(t, mergeRunner, StartInput{
		RunID: mergeRunID, Machine: mergeMachine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule}, RepoRef: repoRef,
	})
	select {
	case <-reviewer.entered:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("merge-review did not acquire the PR branch")
	}

	implementationDone := startRunner(t, implementationRunner, StartInput{
		RunID: implementationRunID, Machine: implementationMachine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRef,
	})
	waitForPreparationRetryEvent(t, runsDir, implementationRunID, "close-out", implementationDone)
	close(reviewer.release)

	assertCompletedRun(t, "merge-review", mergeDone)
	assertCompletedRun(t, "implementation", implementationDone)
	assertNoWorkspaceFailure(t, runsDir, mergeRunID)
	assertNoWorkspaceFailure(t, runsDir, implementationRunID)
}

func TestGatePreparationFailuresConsumeEvaluatorRetryBudget(t *testing.T) {
	g := apiv1.Gate{
		Name:      "review",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Retry: &apiv1.RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1}},
	}
	adjusted := gateWithConsumedPreparationAttempts(g, 2)
	if adjusted.Agentic == nil || adjusted.Agentic.Retry == nil {
		t.Fatal("adjusted gate lost agentic retry policy")
	}
	if adjusted.Agentic.Retry.MaxAttempts != 1 {
		t.Fatalf("remaining evaluator attempts = %d, want 1", adjusted.Agentic.Retry.MaxAttempts)
	}
	if g.Agentic.Retry.MaxAttempts != 3 {
		t.Fatalf("original gate retry policy was mutated to %d", g.Agentic.Retry.MaxAttempts)
	}
}

func compileBranchCollisionMachine(t *testing.T, name string, withReview bool) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "select-pr",
		Tasks: []apiv1.Task{{
			Name: "select-pr", Type: apiv1.TaskDeterministic,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}},
		}},
	}
	if withReview {
		spec.Tasks[0].Next = "review"
		spec.Gates = []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic,
			Agentic:  &apiv1.AgenticGate{Goober: "reviewer", Retry: &apiv1.RetryPolicy{MaxAttempts: 2}},
			Branches: map[string]string{"pass": workflow.TerminalComplete, "needs-changes": workflow.TargetAbort, "fail": workflow.TargetAbort},
		}}
	} else {
		spec.Tasks[0].Next = "close-out"
		spec.Tasks = append(spec.Tasks, apiv1.Task{
			Name: "close-out", Type: apiv1.TaskDeterministic,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}},
		})
	}
	machine, err := workflow.Compile(workflow.Definition{Name: name, Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func startRunner(t *testing.T, runner *Runner, in StartInput) <-chan gatePreparationStartResult {
	t.Helper()
	done := make(chan gatePreparationStartResult, 1)
	go func() {
		result, err := runner.Start(context.Background(), in)
		done <- gatePreparationStartResult{result: result, err: err}
	}()
	return done
}

func assertCompletedRun(t *testing.T, name string, done <-chan gatePreparationStartResult) {
	t.Helper()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("%s Start: %v", name, outcome.err)
		}
		if outcome.result.Phase != journal.PhaseCompleted {
			t.Fatalf("%s phase = %q, want completed", name, outcome.result.Phase)
		}
	case <-time.After(runnerTestWaitTimeout):
		t.Fatalf("%s did not complete", name)
	}
}

func assertNoWorkspaceFailure(t *testing.T, runsDir, runID string) {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Runner[stageErrorCodeKey] == errCodeInfraWorkspace {
			t.Fatalf("run %q recorded %s during branch contention", runID, errCodeInfraWorkspace)
		}
	}
}

func waitForPreparationRetryEvent(t *testing.T, runsDir, runID, stage string, done <-chan gatePreparationStartResult) {
	t.Helper()
	deadline := time.Now().Add(runnerTestWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case outcome := <-done:
			t.Fatalf("run finished before gate preparation retry was observed: result=%+v err=%v", outcome.result, outcome.err)
		default:
		}
		reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
		if err == nil {
			events, eventsErr := reader.Events()
			if eventsErr != nil {
				t.Fatal(eventsErr)
			}
			for _, event := range events {
				if event.Type == journal.EventRunnerAnnotation &&
					event.Stage == stage &&
					event.Runner["kind"] == journal.RetryBackoffKind {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for gate preparation retry backoff event")
}
