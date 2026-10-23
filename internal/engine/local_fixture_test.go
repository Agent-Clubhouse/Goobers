package engine

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type conformanceFixture struct {
	spec         apiv1.WorkflowSpec
	script       map[string][]scriptedCall
	wantLocalErr bool
}

func succeed(outputs map[string]interface{}) scriptedCall {
	return scriptedCall{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: outputs}}
}

func fail(code, message string) scriptedCall {
	return scriptedCall{result: apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: code, Message: message},
	}}
}

func r9Task(name string) apiv1.Task {
	return apiv1.Task{
		Name: name,
		Type: apiv1.TaskDeterministic,
		Goal: name,
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
	}
}

func r9Spec(tasks ...apiv1.Task) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    tasks[0].Name,
		Tasks:    tasks,
	}
}

func runLocalFixture(t *testing.T, fixture conformanceFixture, runID string) []journal.Event {
	t.Helper()
	exec := newScriptedExec(fixture.script)
	instanceRoot := t.TempDir()
	worktrees, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	local, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return exec, nil
		},
		NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			return exec, nil
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  worktrees,
		RunsDir:    runsDir,
		ScratchDir: filepath.Join(instanceRoot, "scratch"),
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	machine, err := wf.Compile(
		wf.Definition{Name: "conformance", Version: 1, Spec: fixture.spec},
		wf.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = local.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub,
			Owner:    "acme",
			Name:     "web",
			Branch:   "main",
		},
	})
	if (err != nil) != fixture.wantLocalErr {
		t.Fatalf("local Start error = %v, wantLocalErr = %t", err, fixture.wantLocalErr)
	}
	return readJournalEvents(t, filepath.Join(runsDir, runID))
}

func readJournalEvents(t *testing.T, dir string) []journal.Event {
	t.Helper()
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead %s: %v", dir, err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("Events %s: %v", dir, err)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("journal %s violates seq monotonicity: %v", dir, err)
	}
	return events
}
