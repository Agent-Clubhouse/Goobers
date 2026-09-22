package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func runnerWorkspaceRevision(owner, name, sha string) *apiv1.WorkspaceRevision {
	return &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderGitHub,
			Owner:    owner,
			Name:     name,
		},
		CommitSHA: sha, SourceRef: "refs/heads/main", SourceID: "source-1",
		BaseRepository: &apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderGitHub, Owner: owner, Name: name,
		},
		BaseSHA: strings.Repeat("b", 40),
	}
}

func readRunnerEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return events
}

func TestRunnerRejectsUnauthorizedWorkspaceRevisionWithoutStageFinished(t *testing.T) {
	const runID = "workspace-revision-unauthorized"
	revision := runnerWorkspaceRevision("other", "repo", strings.Repeat("a", 40))
	r, runsDir := newTestRunner(t, map[string]stubTaskResult{
		runID + ":implement": {
			status:            apiv1.ResultSuccess,
			workspaceRevision: revision,
		},
	}, nil)

	_, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: fixtureMachine(t), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	})
	if err == nil || !strings.Contains(err.Error(), "workspace revision rejected") {
		t.Fatalf("Start error = %v, want workspace revision rejection", err)
	}
	events := readRunnerEvents(t, runsDir, runID)
	var rejection, finished bool
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil &&
			event.Error.Code == "workspace_revision_unauthorized" {
			rejection = true
		}
		if event.Type == journal.EventStageFinished && event.Stage == "implement" {
			finished = true
		}
	}
	if !rejection {
		t.Fatalf("events = %+v, want workspace revision rejection", events)
	}
	if finished {
		t.Fatalf("events = %+v, rejected result must not be persisted as stage.finished", events)
	}
}

type workspaceRevisionResumeDeterministic struct {
	revision *apiv1.WorkspaceRevision
	received chan apiv1.InvocationEnvelope
}

func (d *workspaceRevisionResumeDeterministic) Run(
	_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	if strings.HasSuffix(env.TaskID, ":consume") {
		d.received <- env
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: d.revision.DeepCopy()}, nil
}

func workspaceRevisionResumeMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "workspace-revision-resume", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start: "produce",
			Tasks: []apiv1.Task{
				{Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "approval"},
				{Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TerminalComplete},
			},
			Gates: []apiv1.Gate{{
				Name: "approval", Evaluator: apiv1.EvaluatorHuman,
				Branches: map[string]string{"pass": "consume", "fail": workflow.TargetAbort},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workspace revision resume fixture: %v", err)
	}
	return machine
}

func TestRunnerResumeRestoresFullWorkspaceRevision(t *testing.T) {
	const runID = "workspace-revision-resume"
	revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
	det := &workspaceRevisionResumeDeterministic{
		revision: revision, received: make(chan apiv1.InvocationEnvelope, 1),
	}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return det, nil
	}, nil)
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	paused, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: workspaceRevisionResumeMachine(t), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repo,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if paused.Phase != journal.PhaseRunning {
		t.Fatalf("Start phase = %q, want running at human gate", paused.Phase)
	}
	events := readRunnerEvents(t, runsDir, runID)
	var persisted *apiv1.WorkspaceRevision
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == "produce" {
			persisted = event.WorkspaceRevision
		}
	}
	if persisted == nil || persisted.SourceRef != revision.SourceRef ||
		persisted.CommitSHA != revision.CommitSHA {
		t.Fatalf("persisted revision = %+v, want full verified revision %+v", persisted, revision)
	}
	var pauseSeq uint64
	for _, event := range events {
		if event.Type == journal.EventGatePaused && event.Gate == "approval" {
			pauseSeq = event.Seq
		}
	}
	if pauseSeq == 0 {
		t.Fatalf("events = %+v, want approval pause", events)
	}

	resumed, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: workspaceRevisionResumeMachine(t), RepoRef: repo,
		HumanDecision: &HumanGateDecision{
			Gate: "approval", PauseSeq: pauseSeq, Decision: "pass", Actor: "operator",
		},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume phase = %q, want completed", resumed.Phase)
	}
	select {
	case env := <-det.received:
		if env.WorkspaceRevision == nil || env.WorkspaceRevision.CommitSHA != revision.CommitSHA {
			t.Fatalf("resumed invocation revision = %+v, want %+v", env.WorkspaceRevision, revision)
		}
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("consume invocation was not observed")
	}
}

func TestRunnerParallelConflictingWorkspaceRevisionsFailDeterministically(t *testing.T) {
	const runID = "workspace-revision-parallel-conflict"
	first := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
	second := runnerWorkspaceRevision("acme", "web", strings.Repeat("b", 40))
	byTask := map[string]stubTaskResult{
		runID + ":lens-a":  {status: apiv1.ResultSuccess, workspaceRevision: first},
		runID + ":lens-b":  {status: apiv1.ResultSuccess, workspaceRevision: second},
		runID + ":lens-c":  {status: apiv1.ResultSuccess},
		runID + ":collate": {status: apiv1.ResultSuccess},
	}
	r, _ := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	})
	_, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	})
	if err == nil || !strings.Contains(err.Error(), "reconcile parallel workspace revision") {
		t.Fatalf("Start error = %v, want deterministic parallel revision conflict", err)
	}
}
