package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	}, workflow.WithPreviewFeatures(true))

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

type conflictRemediatingDeterministic struct {
	t                 *testing.T
	remote            string
	root              string
	runDir            string
	baseWork          string
	implementAttempts int
	localCIAttempts   int
}

func (d *conflictRemediatingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	switch {
	case strings.HasSuffix(env.TaskID, ":implement"):
		d.implementAttempts++
		if d.implementAttempts == 1 {
			if err := os.WriteFile(filepath.Join(env.Workspace, "README.md"), []byte("implementation\n"), 0o644); err != nil {
				return apiv1.ResultEnvelope{}, err
			}
			runGit(d.t, env.Workspace, "add", "README.md")
			runGit(d.t, env.Workspace, "commit", "-m", "implement")

			d.baseWork = filepath.Join(d.root, "conflicting-base-work")
			runGit(d.t, "", "clone", d.remote, d.baseWork)
			runGit(d.t, d.baseWork, "config", "user.email", "test@example.com")
			runGit(d.t, d.baseWork, "config", "user.name", "test")
			for i := 1; i <= 15; i++ {
				branch := fmt.Sprintf("sibling-%02d", i)
				runGit(d.t, d.baseWork, "checkout", "-b", branch, "main")
				name := filepath.Join(d.baseWork, branch+".txt")
				if err := os.WriteFile(name, []byte(branch+"\n"), 0o644); err != nil {
					return apiv1.ResultEnvelope{}, err
				}
				runGit(d.t, d.baseWork, "add", filepath.Base(name))
				runGit(d.t, d.baseWork, "commit", "-m", "open "+branch)
				runGit(d.t, d.baseWork, "push", "origin", branch)
			}
			runGit(d.t, d.baseWork, "checkout", "main")
			if err := os.WriteFile(filepath.Join(d.baseWork, "README.md"), []byte("base round 1\n"), 0o644); err != nil {
				return apiv1.ResultEnvelope{}, err
			}
			runGit(d.t, d.baseWork, "add", "README.md")
			runGit(d.t, d.baseWork, "commit", "-m", "advance base during local-ci")
			runGit(d.t, d.baseWork, "push", "origin", "main")
			break
		}

		var conflictPointer *apiv1.ArtifactPointer
		for i := range env.ContextPointers {
			if env.ContextPointers[i].Name == "local-ci.artifact[0]" {
				conflictPointer = env.ContextPointers[i].Artifact
				break
			}
		}
		if conflictPointer == nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("remediation invocation missing local-ci conflict context: %+v", env.ContextPointers)
		}
		conflictData, err := conflictPointer.Resolve(d.runDir)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve local-ci conflict context: %w", err)
		}
		var conflict baseSyncConflictArtifact
		if err := json.Unmarshal(conflictData, &conflict); err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("decode local-ci conflict context: %w", err)
		}
		if conflict.Code != baseSyncConflictErrorCode || conflict.Branch == "" || conflict.BaseRef != "main" ||
			len(conflict.ConflictingFiles) != 1 || conflict.ConflictingFiles[0] != "README.md" {
			return apiv1.ResultEnvelope{}, fmt.Errorf("local-ci conflict context = %+v, want README.md conflict against main", conflict)
		}

		cmd := exec.Command("git", "merge", "--no-edit", "main")
		cmd.Dir = env.Workspace
		if out, err := cmd.CombinedOutput(); err == nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("remediation merge unexpectedly succeeded: %s", out)
		}
		if err := os.WriteFile(filepath.Join(env.Workspace, "README.md"), []byte("resolved\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(d.t, env.Workspace, "add", "README.md")
		runGit(d.t, env.Workspace, "commit", "-m", "resolve base conflict")
		if d.implementAttempts == 2 {
			if err := os.WriteFile(filepath.Join(d.baseWork, "README.md"), []byte("base round 2\n"), 0o644); err != nil {
				return apiv1.ResultEnvelope{}, err
			}
			runGit(d.t, d.baseWork, "add", "README.md")
			runGit(d.t, d.baseWork, "commit", "-m", "advance base again during local-ci")
			runGit(d.t, d.baseWork, "push", "origin", "main")
		}
	case strings.HasSuffix(env.TaskID, ":local-ci"):
		d.localCIAttempts++
		content, err := os.ReadFile(filepath.Join(env.Workspace, "README.md"))
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		if string(content) != "resolved\n" {
			return apiv1.ResultEnvelope{}, fmt.Errorf("local-ci README = %q, want resolved content", content)
		}
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestRunnerRoutesBatchLoadBaseSyncConflictsThroughBoundedRemediation(t *testing.T) {
	remote := newFixtureRepo(t)
	deterministic := &conflictRemediatingDeterministic{t: t, remote: remote, root: t.TempDir()}
	r, runsDir := newWorktreeProvisioningTestRunner(t, remote, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return deterministic, nil
	})
	deterministic.runDir = filepath.Join(runsDir, "run-sync-conflict")
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
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, SyncBase: true}, Next: "local-gate",
				},
			},
			Gates: []apiv1.Gate{{
				Name:      "local-gate",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass":                  workflow.TerminalComplete,
					"fail":                  "implement",
					workflow.BranchEscalate: workflow.TargetEscalate,
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID:   "run-sync-conflict",
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
	if deterministic.implementAttempts != 3 {
		t.Fatalf("implement attempts = %d, want 3", deterministic.implementAttempts)
	}
	if deterministic.localCIAttempts != 1 {
		t.Fatalf("local-ci executor attempts = %d, want 1 after repeated conflict remediation", deterministic.localCIAttempts)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-sync-conflict"))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var conflictCount int
	var successfulCI bool
	var retryAttempts []int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "run_failed" {
			t.Fatalf("batch-load sync conflict terminated the run: %+v", event.Error)
		}
		if event.Stage == "local-ci" && event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error" {
			t.Fatalf("base sync conflict was journaled as executor_error: %+v", event.Error)
		}
		if event.Stage == "local-ci" && event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) &&
			event.Error != nil && event.Error.Code == baseSyncConflictErrorCode {
			conflictCount++
		}
		if event.Stage == "local-ci" && event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultSuccess) {
			successfulCI = true
		}
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == retryDecisionKind &&
			event.Runner["failureCode"] == baseSyncConflictErrorCode {
			if event.Runner[retryFailureClassKey] != string(journal.AttemptPolicy) {
				t.Fatalf("retry failure class = %v, want %s", event.Runner[retryFailureClassKey], journal.AttemptPolicy)
			}
			retryAttempts = append(retryAttempts, int(event.Runner["repassAttempt"].(float64)))
		}
	}
	if conflictCount != 2 {
		t.Fatalf("typed base-sync conflicts = %d, want 2", conflictCount)
	}
	if !successfulCI {
		t.Fatal("journal missing successful local-ci after remediation")
	}
	if len(retryAttempts) != 2 || retryAttempts[0] != 1 || retryAttempts[1] != 2 {
		t.Fatalf("journaled retry attempts = %v, want [1 2]", retryAttempts)
	}
}
