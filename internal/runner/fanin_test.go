package runner

import (
	"context"
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type capturingFanInExecutor struct {
	delegate *stubDeterministic
	envs     map[string]apiv1.InvocationEnvelope
}

func (c *capturingFanInExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.envs[env.TaskID] = env
	return c.delegate.Run(ctx, env, run)
}

func fanInMachine(t *testing.T, failedPerformance bool) *workflow.Machine {
	t.Helper()
	branchTask := func(name string, continueOnError bool) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			ExpectedOutputs: []string{"summary"},
			ContinueOnError: continueOnError,
			Next:            workflow.TargetJoin,
		}
	}
	tasks := []apiv1.Task{
		branchTask("review-security", false),
		branchTask("review-performance", false),
	}
	branches := []apiv1.Branch{
		{Name: "security", Start: "review-security"},
		{Name: "performance", Start: "review-performance"},
	}
	inputsFrom := map[string]string{
		"security":    "fan.security.review-security.summary",
		"performance": "fan.performance.summary",
	}
	if failedPerformance {
		inputsFrom[BranchCompletenessInput] = "fan.performance.summary"
	} else {
		inputsFrom[BranchCompletenessInput] = "fan.security.summary"
		tasks = append(tasks, branchTask("review-reliability", false))
		branches = append(branches, apiv1.Branch{Name: "reliability", Start: "review-reliability"})
		inputsFrom["reliability"] = "fan.reliability.review-reliability.summary"
	}
	tasks = append(tasks, apiv1.Task{
		Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate reports",
		Run:        &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		InputsFrom: inputsFrom,
		Next:       workflow.TerminalComplete,
	})
	machine, err := workflow.Compile(workflow.Definition{
		Name: "fan-in", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks:    tasks,
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				Branches: branches, Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile fan-in machine: %v", err)
	}
	return machine
}

func skippedJoinMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "skipped-join", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "prepare",
			Tasks: []apiv1.Task{
				task("prepare", "fan"),
				task("review-security", workflow.TargetJoin),
				task("review-performance", workflow.TargetJoin),
				task("collate", workflow.TerminalComplete),
				task("recover", workflow.TerminalComplete),
			},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchFailFast, Join: "collate", OnFailure: "recover",
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "performance", Start: "review-performance"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile skipped-join machine: %v", err)
	}
	return machine
}

func TestRunnerFansInThreeBranchScalarsAndArtifacts(t *testing.T) {
	envs := map[string]apiv1.InvocationEnvelope{}
	results := map[string]stubTaskResult{}
	for _, runID := range []string{"fan-in-a", "fan-in-b"} {
		for _, branch := range []string{"security", "performance", "reliability"} {
			results[runID+":review-"+branch] = stubTaskResult{
				status: apiv1.ResultSuccess, outputs: map[string]interface{}{"summary": branch},
				artifactName: branch + ".md", artifactData: []byte(branch), artifactMediaType: "text/markdown",
			}
		}
		results[runID+":collate"] = stubTaskResult{status: apiv1.ResultSuccess}
	}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &capturingFanInExecutor{
			delegate: &stubDeterministic{rec: rec, byTask: results},
			envs:     envs,
		}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	for _, runID := range []string{"fan-in-a", "fan-in-b"} {
		result, err := r.Start(context.Background(), StartInput{
			RunID: runID, Machine: fanInMachine(t, false), Gaggle: "goobers",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		if err != nil {
			t.Fatalf("%s: Start: %v", runID, err)
		}
		if result.Phase != journal.PhaseCompleted {
			t.Fatalf("%s: phase = %q, want completed", runID, result.Phase)
		}
	}

	first := envs["fan-in-a:collate"]
	for _, branch := range []string{"security", "performance", "reliability"} {
		if got := first.Inputs[branch]; got != branch {
			t.Errorf("join input %q = %#v, want %q", branch, got, branch)
		}
	}
	if len(first.ContextPointers) != 3 {
		t.Fatalf("join pointers = %+v, want one artifact from each branch", first.ContextPointers)
	}
	for i, branch := range []string{"security", "performance", "reliability"} {
		pointer := first.ContextPointers[i]
		if pointer.Branch != i+1 || pointer.BranchName != branch || pointer.Artifact == nil {
			t.Errorf("pointer %d = %+v, want artifact tagged branch %d/%q", i, pointer, i+1, branch)
		}
	}
	completeness, ok := first.Inputs[BranchCompletenessInput].([]journal.BranchOutcome)
	if !ok || len(completeness) != 3 {
		t.Fatalf("branch completeness = %#v, want three outcomes", first.Inputs[BranchCompletenessInput])
	}
	for i, branch := range completeness {
		if branch.Branch != i+1 || branch.Status != journal.BranchSucceeded || branch.Artifacts != 1 {
			t.Errorf("completeness %d = %+v, want succeeded branch with one artifact", i, branch)
		}
	}

	firstPointers, err := json.Marshal(first.ContextPointers)
	if err != nil {
		t.Fatal(err)
	}
	secondPointers, err := json.Marshal(envs["fan-in-b:collate"].ContextPointers)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstPointers) != string(secondPointers) {
		t.Errorf("journaled pointer sets differ across runs:\n%s\n%s", firstPointers, secondPointers)
	}
}

func TestRunnerOmitsFailedBranchInputAndReportsCompleteness(t *testing.T) {
	envs := map[string]apiv1.InvocationEnvelope{}
	results := map[string]stubTaskResult{
		"fan-in-failed:review-security": {
			status: apiv1.ResultSuccess, outputs: map[string]interface{}{"summary": "security"},
		},
		"fan-in-failed:review-performance": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "review_failed", Message: "performance review failed"},
		},
		"fan-in-failed:collate": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &capturingFanInExecutor{
			delegate: &stubDeterministic{rec: rec, byTask: results},
			envs:     envs,
		}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	_, err := r.Start(context.Background(), StartInput{
		RunID: "fan-in-failed", Machine: fanInMachine(t, true), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	join := envs["fan-in-failed:collate"]
	if _, ok := join.Inputs["performance"]; ok {
		t.Errorf("failed branch input should be absent, got %#v", join.Inputs["performance"])
	}
	if got := join.Inputs["security"]; got != "security" {
		t.Errorf("successful branch input = %#v, want security", got)
	}
	completeness, ok := join.Inputs[BranchCompletenessInput].([]journal.BranchOutcome)
	if !ok || len(completeness) != 2 || completeness[1].Status != journal.BranchFailed {
		t.Errorf("branch completeness = %#v, want failed performance branch", join.Inputs[BranchCompletenessInput])
	}
}

func TestRunnerResumeAfterSkippedJoinKeepsOnlyRootPointers(t *testing.T) {
	machine := skippedJoinMachine(t)
	capturing := &capturingDeterministic{}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return capturing, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "fan-in-skipped-resume", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("recover")
	rootRef, err := jr.RecordArtifact("prepare.md", []byte("prepare"))
	if err != nil {
		t.Fatalf("record root artifact: %v", err)
	}
	branchRef, err := jr.RecordArtifact("security.md", []byte("security"))
	if err != nil {
		t.Fatalf("record branch artifact: %v", err)
	}
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "prepare", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "prepare", Attempt: 1, Status: string(apiv1.ResultSuccess),
			Artifacts: []journal.Ref{{Path: rootRef.Path, Digest: rootRef.Digest, Size: rootRef.Size}}},
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"}, {Branch: 2, Name: "performance"},
		}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security", Stage: "review-security"},
		{Type: journal.EventStageStarted, Stage: "review-security", Attempt: 1, Branch: 1},
		{Type: journal.EventStageFinished, Stage: "review-security", Attempt: 1, Branch: 1, Status: string(apiv1.ResultFailure),
			Artifacts: []journal.Ref{{Path: branchRef.Path, Digest: branchRef.Digest, Size: branchRef.Size}}},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 1, BranchName: "security", BranchStatus: journal.BranchFailed},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 2, BranchName: "performance", BranchStatus: journal.BranchCancelled},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: "recover", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security", Status: journal.BranchFailed, Artifacts: 1},
			{Branch: 2, Name: "performance", Status: journal.BranchCancelled},
		}},
	}
	for _, event := range events {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: "fan-in-skipped-resume", Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	if capturing.calls != 1 {
		t.Fatalf("onFailure calls = %d, want 1", capturing.calls)
	}
	if len(capturing.lastEnv.ContextPointers) != 1 {
		t.Fatalf("onFailure pointers = %+v, want only the root pointer", capturing.lastEnv.ContextPointers)
	}
	pointer := capturing.lastEnv.ContextPointers[0]
	if pointer.Name != "prepare.artifact[0]" || pointer.Artifact == nil || pointer.Artifact.Digest != rootRef.Digest {
		t.Fatalf("onFailure pointer = %+v, want the root prepare artifact", pointer)
	}
}

func TestReconstructParallelPointersAndPendingFanIn(t *testing.T) {
	machine := fanInMachine(t, true)
	events := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"}, {Branch: 2, Name: "performance"},
		}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 2, BranchName: "performance"},
		{Type: journal.EventStageFinished, Stage: "review-performance", Branch: 2,
			Outputs:   map[string]any{"summary": "performance"},
			Artifacts: []journal.Ref{{Path: "performance.md", Digest: "sha256:performance"}}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security"},
		{Type: journal.EventStageFinished, Stage: "review-security", Branch: 1,
			Outputs:   map[string]any{"summary": "security"},
			Artifacts: []journal.Ref{{Path: "security.md", Digest: "sha256:security"}}},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: "collate", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security", Status: journal.BranchSucceeded, Artifacts: 1},
			{Branch: 2, Name: "performance", Status: journal.BranchFailed, Artifacts: 1},
		}},
	}

	pointers := reconstructPointers(events, machine)
	if len(pointers) != 2 || pointers[0].BranchName != "security" || pointers[1].BranchName != "performance" {
		t.Fatalf("reconstructed pointers = %+v, want declaration order with branch attribution", pointers)
	}
	fanIn := pendingFanIn(events, machine)
	if fanIn == nil || fanIn.branch("performance").status != journal.BranchFailed {
		t.Fatalf("pending fan-in = %+v, want failed performance outcome restored", fanIn)
	}
	completed := reconstructStageOutputs(events)
	if got, ok, branchRef, absent := resolveBranchInput(
		"fan.security.review-security.summary", machine, completed, fanIn,
	); !branchRef || absent || !ok || got != "security" {
		t.Errorf("resumed security input = %#v (ok=%v branchRef=%v absent=%v), want security", got, ok, branchRef, absent)
	}
	if _, ok, branchRef, absent := resolveBranchInput(
		"fan.performance.summary", machine, completed, fanIn,
	); !branchRef || !absent || ok {
		t.Errorf("resumed failed branch = ok=%v branchRef=%v absent=%v, want absent", ok, branchRef, absent)
	}
}
