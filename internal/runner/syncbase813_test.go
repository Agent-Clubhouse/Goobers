package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type syncBaseDeterministic struct {
	t      *testing.T
	remote string
	root   string
}

func (d *syncBaseDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	switch {
	case strings.HasSuffix(env.TaskID, ":implement"):
		if err := os.WriteFile(filepath.Join(env.Workspace, "implementation.txt"), []byte("run change\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(d.t, env.Workspace, "add", "implementation.txt")
		runGit(d.t, env.Workspace, "commit", "-m", "implement")

		baseWork := filepath.Join(d.root, "base-work")
		runGit(d.t, "", "clone", d.remote, baseWork)
		runGit(d.t, baseWork, "config", "user.email", "test@example.com")
		runGit(d.t, baseWork, "config", "user.name", "test")
		if err := os.WriteFile(filepath.Join(baseWork, "build-fix.txt"), []byte("late build behavior fix\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(d.t, baseWork, "add", "build-fix.txt")
		runGit(d.t, baseWork, "commit", "-m", "fix build behavior")
		runGit(d.t, baseWork, "push", "origin", "main")
	case strings.HasSuffix(env.TaskID, ":local-ci"):
		for _, name := range []string{"implementation.txt", "build-fix.txt"} {
			if _, err := os.Stat(filepath.Join(env.Workspace, name)); err != nil {
				return apiv1.ResultEnvelope{}, fmt.Errorf("local-ci workspace missing %s: %w", name, err)
			}
		}
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestRunnerSyncsConfiguredStageWithLatestBase(t *testing.T) {
	remote := newFixtureRepo(t)
	deterministic := &syncBaseDeterministic{t: t, remote: remote, root: t.TempDir()}
	r, _ := newWorktreeProvisioningTestRunner(t, remote, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return deterministic, nil
	})
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "implementation",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "implement",
			Tasks: []apiv1.Task{
				{
					Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "local-ci",
				},
				{
					Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "test",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, SyncBase: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID:   "run-sync-base",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %s, want %s", result.Phase, journal.PhaseCompleted)
	}
}
