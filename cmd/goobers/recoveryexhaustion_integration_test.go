//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type recoveryExhaustionExecutor struct {
	t                     *testing.T
	unchanged             bool
	implement, ci, opened int
	workspaces            []string
}

func (e *recoveryExhaustionExecutor) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if env.Workspace != "" {
		e.workspaces = append(e.workspaces, env.Workspace)
	}
	switch {
	case strings.HasSuffix(env.TaskID, ":implement"):
		e.implement++
		if !e.unchanged || e.implement == 1 {
			if err := os.WriteFile(filepath.Join(env.Workspace, "implementation.txt"), []byte(fmt.Sprintf("implementation %d", e.implement)), 0o600); err != nil {
				return apiv1.ResultEnvelope{}, err
			}
			recoveryCLIGit(e.t, env.Workspace, "add", "implementation.txt")
			recoveryCLIGit(e.t, env.Workspace, "commit", "-m", "implementation")
		}
	case strings.HasSuffix(env.TaskID, ":local-ci"):
		e.ci++
		return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "test_failure", Message: "deterministic local CI failure"}}, nil
	case strings.HasSuffix(env.TaskID, ":open-pr"):
		e.opened++
	default:
		return apiv1.ResultEnvelope{}, fmt.Errorf("unexpected task %s", env.TaskID)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

type recoveryExhaustionReviewer struct {
	unchanged bool
	calls     int
}

func (*recoveryExhaustionReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *recoveryExhaustionReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.calls++
	decision := apiv1.VerdictPass
	if r.unchanged {
		decision = apiv1.VerdictNeedsChanges
	}
	return apiv1.Verdict{Decision: decision, Summary: "fixture review"}, nil
}

func TestIntegrationRecoverySurvivesPrePRExhaustion(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"local-ci", "unchanged-repass"} {
		t.Run(mode, func(t *testing.T) { testPrePRRecovery(t, mode) })
	}
}

func testPrePRRecovery(t *testing.T, mode string) {
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	cloneURL := func(apiv1.RepoRef) (string, error) { return source, nil }
	previous := repoCloneURL
	repoCloneURL = cloneURL
	t.Cleanup(func() { repoCloneURL = previous })
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, layout.WorkcopiesDir(), cloneURL, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	runID := "prepr-" + mode
	configured := cfg.Repos[0]
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(configured.Provider), Owner: configured.Owner, Project: configured.Project, Name: configured.Name, URL: configured.BaseURL}
	seedItemRepositoryForTest(t, layout, runID, "7", repo)
	executor := &recoveryExhaustionExecutor{t: t, unchanged: mode == "unchanged-repass"}
	reviewer := &recoveryExhaustionReviewer{unchanged: executor.unchanged}
	finalized := false
	var finalizationErr error
	r, err := runner.New(runner.Config{
		RunsDir: layout.RunsDir(), Worktrees: manager, RepoCloneURL: cloneURL,
		MaxRepasses: 1, Automated: gate.NewAutomatedEvaluator(), RecoveryEvents: recoveryRunEvents(layout),
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return executor, nil
		},
		NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		FinalizeTerminal: func(id string, _ journal.RunPhase) error {
			finalized = true
			finalizationErr = finalizeTerminalRunWithClaimRelease(layout, nil, manager, id, func(instance.Layout, *journal.InstanceLog, string) error { return nil })
			return finalizationErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := r.Start(ctx, runner.StartInput{RunID: runID, Machine: recoveryExhaustionMachine(t), Gaggle: "example", RepoRef: apiv1.RepoRef{Provider: apiv1.Provider(configured.Provider), Owner: configured.Owner, Name: configured.Name, Branch: "main"}})
	if err != nil || result.Phase != journal.PhaseEscalated {
		t.Fatalf("pre-PR run: %+v %v", result, err)
	}
	if executor.opened != 0 || executor.implement != 2 {
		t.Fatalf("unexpected stage counts: %+v", executor)
	}
	if !finalized || finalizationErr != nil {
		t.Fatalf("terminal cleanup did not complete: called=%t error=%v", finalized, finalizationErr)
	}
	for _, path := range executor.workspaces {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("terminal cleanup left execution workspace: %v", err)
		}
	}
	if !executor.unchanged && (executor.ci != 2 || reviewer.calls != 2) {
		t.Fatalf("local-CI budget was not exercised after two passing reviews: CI=%d reviews=%d", executor.ci, reviewer.calls)
	}
	if executor.unchanged && reviewer.calls != 1 {
		t.Fatalf("duplicate diff reached reviewer again: %d", reviewer.calls)
	}
	wantReason := gate.ReasonRepassBudgetExhausted
	wantContent := "implementation 2"
	if executor.unchanged {
		wantReason, wantContent = gate.ReasonUnchangedRepass, "implementation 1"
	}
	reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	sawReason := false
	for _, event := range events {
		if event.Runner["reason"] == wantReason {
			sawReason = true
		}
	}
	if !sawReason {
		t.Fatalf("run did not exercise %s", wantReason)
	}
	selected, err := selectIssueRecovery(ctx, layout, repo.CanonicalKey(), "7", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if selected.Record.RunID != runID || !selected.Record.RetainUntil.After(time.Now().Add(29*24*time.Hour)) {
		t.Fatal("terminal cleanup lost the recovery window")
	}
	resumeExhaustedRecovery(t, layout, source, repo, wantContent)
}

func recoveryExhaustionMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{Gaggle: "example", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
			{Name: "local-ci", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "local-gate"},
			{Name: "open-pr", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		}, Gates: []apiv1.Gate{
			{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}, Branches: map[string]string{"pass": "local-ci", "needs-changes": "implement", "fail": workflow.TargetAbort}},
			{Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "failure-class"}, Branches: map[string]string{"pass": "open-pr", "fail": "implement", "infra": "local-ci"}},
		}}
	machine, err := workflow.Compile(workflow.Definition{Name: "implementation", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func resumeExhaustedRecovery(t *testing.T, layout instance.Layout, source string, repo providers.RepositoryRef, want string) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", "receiving-run", "implementation-recovery", time.Hour); err != nil || !ok {
		t.Fatalf("receiving claim: %t %v", ok, err)
	}
	seedItemRepositoryForTest(t, layout, "receiving-run", "7", repo)
	destination := t.TempDir()
	recoveryCLIGit(t, destination, "clone", source, ".")
	remote, err := runner.DefaultRepoCloneURL(apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name, BaseURL: repo.URL})
	if err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, destination, "config", "url."+source+".insteadOf", remote)
	recoveryCLIGit(t, destination, "checkout", "-b", providers.BranchName("implementation-recovery", "receiving-run"))
	t.Setenv("GOOBERS_RUN_ID", "receiving-run")
	t.Setenv("GOOBERS_WORKFLOW", "implementation-recovery")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "fixture-only-token")
	t.Chdir(destination)
	if code := runRecoveryResume([]string{layout.Root}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("resume returned %d", code)
	}
	data, err := os.ReadFile(filepath.Join(destination, "implementation.txt"))
	if err != nil || string(data) != want {
		t.Fatalf("restored implementation = %q %v; want %q", data, err, want)
	}
}
