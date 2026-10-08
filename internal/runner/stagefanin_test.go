package runner

// #5893: a fan-in/adjudication stage reads earlier stages' reports through the
// run contract — each producer's artifact becomes a context pointer that
// accumulates across linear stages — not through outbox or its host mirror.
// This pins the documented review-panel shape: several sequential reviewers,
// then a governor naming them with contextFrom.

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type fanInDeterministic struct {
	rec  ArtifactRecorder
	envs []apiv1.InvocationEnvelope
}

func (f *fanInDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	f.envs = append(f.envs, env)
	stage := envelopeStage(env)
	if stage == "govern" {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "adjudicated"}, nil
	}
	ref, err := f.rec.RecordArtifact(stage+".md", []byte("report from "+stage))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess,
		Artifacts: []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
			MediaType: "text/markdown", Integrity: ref.Integrity,
		}},
	}, nil
}

func fanInPanelMachine(t *testing.T, reviewers []string) *workflow.Machine {
	t.Helper()
	stage := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	tasks := []apiv1.Task{stage("prepare", reviewers[0])}
	for i, name := range reviewers {
		next := "govern"
		if i+1 < len(reviewers) {
			next = reviewers[i+1]
		}
		tasks = append(tasks, stage(name, next))
	}
	govern := stage("govern", "")
	govern.ContextFrom = append([]string(nil), reviewers...)
	tasks = append(tasks, govern)
	m, err := workflow.Compile(
		workflow.Definition{Name: "review-panel-fan-in", Version: 1, Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start: "prepare", Tasks: tasks,
		}},
		workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile review panel: %v", err)
	}
	return m
}

func TestRunnerFanInStageReceivesEveryNamedReviewerArtifact(t *testing.T) {
	const runID = "run-review-panel-fan-in"
	reviewers := []string{"review-a", "review-b", "review-c", "review-d", "review-e", "review-f"}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	deterministic := &fanInDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			deterministic.rec = rec
			return deterministic, nil
		},
		Worktrees:  wtMgr,
		RunsDir:    runsDir,
		ScratchDir: filepath.Join(instanceRoot, "scratch"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{RunID: runID, Machine: fanInPanelMachine(t, reviewers), Gaggle: "acme-web"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	var governEnv *apiv1.InvocationEnvelope
	for i := range deterministic.envs {
		if envelopeStage(deterministic.envs[i]) == "govern" {
			governEnv = &deterministic.envs[i]
		}
	}
	if governEnv == nil {
		t.Fatal("govern was never dispatched")
	}
	want := make([]string, 0, len(reviewers))
	for _, name := range reviewers {
		want = append(want, name+".artifact[0]")
	}
	if got := pointerNames(governEnv.ContextPointers); !reflect.DeepEqual(got, want) {
		t.Fatalf("govern context pointers = %v, want exactly every reviewer report in order %v (and not prepare's)", got, want)
	}
	for i, pointer := range governEnv.ContextPointers {
		if pointer.Artifact == nil {
			t.Fatalf("pointer %q carries no artifact", pointer.Name)
		}
		data, err := pointer.Artifact.Resolve(filepath.Join(runsDir, runID))
		if err != nil {
			t.Fatalf("resolve %q: %v", pointer.Name, err)
		}
		if got, wantBody := string(data), "report from "+reviewers[i]; got != wantBody {
			t.Fatalf("pointer %q content = %q, want %q", pointer.Name, got, wantBody)
		}
	}
}
