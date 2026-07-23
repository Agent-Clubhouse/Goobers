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
	runDir            string
	implementAttempts int
	localCIAttempts   int
}

type countingAutomated struct {
	delegate invoke.Automated
	calls    int
}

func (a *countingAutomated) Evaluate(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	a.calls++
	return a.delegate.Evaluate(ctx, gate, env)
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

const batchLoadPullRequests = 19

type batchLoadRepo struct {
	t           *testing.T
	remote      string
	runDir      string
	pullHeads   []string
	cloneCalls  int
	maxLandings int
	landedPulls int
}

func newBatchLoadRepo(t *testing.T, maxLandings int) *batchLoadRepo {
	t.Helper()
	remote := newFixtureRepo(t)
	work := filepath.Join(t.TempDir(), "sibling-pulls")
	runGit(t, "", "clone", remote, work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")

	repo := &batchLoadRepo{t: t, remote: remote, maxLandings: maxLandings}
	for i := 1; i <= batchLoadPullRequests; i++ {
		branch := fmt.Sprintf("sibling-%02d", i)
		runGit(t, work, "checkout", "-b", branch)
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(fmt.Sprintf("base round %d\n", i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", branch, err)
		}
		runGit(t, work, "add", "README.md")
		runGit(t, work, "commit", "-m", "open "+branch)
		head := gitOutput(t, work, "rev-parse", "HEAD")
		repo.pullHeads = append(repo.pullHeads, head)
		runGit(t, work, "push", "origin", "HEAD:refs/heads/"+branch)
		runGit(t, work, "push", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", i))
	}
	return repo
}

// cloneURL is called after stage.started and before worktree.Create fetches.
// Landing the next sibling here makes the base move during local-ci
// provisioning, rather than before the stage begins.
func (r *batchLoadRepo) cloneURL(apiv1.RepoRef) (string, error) {
	r.cloneCalls++
	if r.cloneCalls%2 == 0 && r.landedPulls < r.maxLandings {
		rd, err := journal.OpenRead(r.runDir)
		if err != nil {
			r.t.Fatalf("open active journal before sibling landing: %v", err)
		}
		events, err := rd.Events()
		if err != nil {
			r.t.Fatalf("read active journal before sibling landing: %v", err)
		}
		var localCIStarts int
		for _, event := range events {
			if event.Type == journal.EventStageStarted && event.Stage == "local-ci" {
				localCIStarts++
			}
		}
		if localCIStarts != r.landedPulls+1 {
			r.t.Fatalf("local-ci starts before sibling landing = %d, want %d", localCIStarts, r.landedPulls+1)
		}
		r.landedPulls++
		pull := r.landedPulls
		runGit(r.t, "", "--git-dir="+r.remote, "update-ref", "refs/heads/main", r.pullHeads[pull-1])
		runGit(r.t, "", "--git-dir="+r.remote, "update-ref", "-d", fmt.Sprintf("refs/pull/%d/head", pull))
	}
	return r.remote, nil
}

func (r *batchLoadRepo) openPulls() int {
	out := gitOutput(r.t, "", "--git-dir="+r.remote, "for-each-ref", "--format=%(refname)", "refs/pull/")
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRunnerRoutesBatchLoadBaseSyncConflictsThroughBoundedRemediation(t *testing.T) {
	tests := []struct {
		name              string
		landings          int
		wantPhase         journal.RunPhase
		wantImplement     int
		wantLocalCI       int
		wantGateCalls     int
		wantRetryAttempts []int
	}{
		{
			name:              "converges_at_budget",
			landings:          3,
			wantPhase:         journal.PhaseCompleted,
			wantImplement:     4,
			wantLocalCI:       1,
			wantGateCalls:     1,
			wantRetryAttempts: []int{1, 2, 3},
		},
		{
			name:              "escalates_after_budget",
			landings:          4,
			wantPhase:         journal.PhaseEscalated,
			wantImplement:     4,
			wantLocalCI:       0,
			wantGateCalls:     0,
			wantRetryAttempts: []int{1, 2, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newBatchLoadRepo(t, tt.landings)
			if got := repo.openPulls(); got != batchLoadPullRequests {
				t.Fatalf("initial open sibling pull refs = %d, want %d", got, batchLoadPullRequests)
			}
			deterministic := &conflictRemediatingDeterministic{t: t}
			r, runsDir := newWorktreeProvisioningTestRunner(t, repo.remote, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
				return deterministic, nil
			})
			automated := &countingAutomated{delegate: r.cfg.Automated}
			r.cfg.Automated = automated
			r.cfg.RepoCloneURL = repo.cloneURL
			runID := "run-sync-conflict-" + tt.name
			deterministic.runDir = filepath.Join(runsDir, runID)
			repo.runDir = deterministic.runDir
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
				RunID:   runID,
				Machine: machine,
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if result.Phase != tt.wantPhase {
				t.Fatalf("phase = %s, want %s", result.Phase, tt.wantPhase)
			}
			if deterministic.implementAttempts != tt.wantImplement {
				t.Fatalf("implement attempts = %d, want %d", deterministic.implementAttempts, tt.wantImplement)
			}
			if deterministic.localCIAttempts != tt.wantLocalCI {
				t.Fatalf("local-ci executor attempts = %d, want %d", deterministic.localCIAttempts, tt.wantLocalCI)
			}
			if automated.calls != tt.wantGateCalls {
				t.Fatalf("automated gate calls = %d, want %d; classified conflicts must take the known retry outcome", automated.calls, tt.wantGateCalls)
			}
			if got, want := repo.openPulls(), batchLoadPullRequests-tt.landings; got != want {
				t.Fatalf("open sibling pull refs after %d landings = %d, want %d", tt.landings, got, want)
			}

			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
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
					t.Fatalf("batch-load sync conflict terminated the run fatally: %+v", event.Error)
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
			if conflictCount != tt.landings {
				t.Fatalf("typed base-sync conflicts = %d, want %d", conflictCount, tt.landings)
			}
			if successfulCI != (tt.wantLocalCI > 0) {
				t.Fatalf("successful local-ci = %t, want %t", successfulCI, tt.wantLocalCI > 0)
			}
			if len(retryAttempts) != len(tt.wantRetryAttempts) {
				t.Fatalf("journaled retry attempts = %v, want %v", retryAttempts, tt.wantRetryAttempts)
			}
			for i := range retryAttempts {
				if retryAttempts[i] != tt.wantRetryAttempts[i] {
					t.Fatalf("journaled retry attempts = %v, want %v", retryAttempts, tt.wantRetryAttempts)
				}
			}
		})
	}
}
