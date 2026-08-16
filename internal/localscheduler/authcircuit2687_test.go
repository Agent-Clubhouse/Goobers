package localscheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestAuthFailureCircuitStopsBacklogPollingUntilReload(t *testing.T) {
	authErr := errors.New("GET /commits/abc/check-runs failed: status 403: Resource not accessible by personal access token")
	failing := &fakeBacklogCounter{err: authErr}
	entry := WorkflowEntry{
		Workflow:              "implementation",
		Gaggle:                "goobers-site",
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		BacklogCounter:        failing,
		ScheduleDemandCounter: failing,
		Starter:               &fakeStarter{},
	}
	sched, dir := newTestScheduler(t, []WorkflowEntry{entry})
	now := time.Now()

	sched.Tick(context.Background(), now.Add(2*time.Hour))
	sched.Tick(context.Background(), now.Add(3*time.Hour))
	if got := failing.polls(); got != 1 {
		t.Fatalf("polls after permanent auth failure = %d, want 1", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var authEvents int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == providers.ErrorCodeAuthFailed {
			authEvents++
		}
	}
	if authEvents != 1 {
		t.Fatalf("github_auth_failed events = %d, want 1: %+v", authEvents, events)
	}

	repaired := &fakeBacklogCounter{}
	entry.BacklogCounter = repaired
	entry.Schedules = nil
	entry.ScheduleDemandCounter = nil
	if err := sched.Reload([]WorkflowEntry{entry}, nil, now, "old", "new"); err != nil {
		t.Fatal(err)
	}
	sched.Tick(context.Background(), now.Add(4*time.Hour))
	if got := repaired.polls(); got != 1 {
		t.Fatalf("polls after credential configuration reload = %d, want 1", got)
	}
}

func TestAuthFailureCircuitStopsRunRedispatch(t *testing.T) {
	for _, failureCode := range []string{
		providers.ErrorCodeAuthFailed,
		telemetry.ErrCodeCredentialUnavailable,
	} {
		t.Run(failureCode, func(t *testing.T) {
			starter := &fakeStarter{result: StartResult{
				Phase:          journal.PhaseFailed,
				FailureStage:   "query-backlog",
				FailureCode:    failureCode,
				FailureMessage: "permission denied",
			}}
			sched, _ := newTestScheduler(t, []WorkflowEntry{{
				Workflow:  "implementation",
				Gaggle:    "goobers-site",
				Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
				Starter:   starter,
			}})

			if _, err := sched.Trigger(context.Background(), "implementation", time.Now()); err != nil {
				t.Fatal(err)
			}
			identity := WorkflowIdentity{Gaggle: "goobers-site", Workflow: "implementation"}
			waitForCount(t, func() int {
				if sched.authCircuitOpen(identity) {
					return 1
				}
				return 0
			}, 1)

			_, err := sched.Trigger(context.Background(), "implementation", time.Now())
			var rejected *TriggerRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("second trigger error = %v, want TriggerRejectedError", err)
			}
			if !strings.HasPrefix(rejected.Reason, ReasonProviderAuth) {
				t.Fatalf("second trigger reason = %q, want %q prefix", rejected.Reason, ReasonProviderAuth)
			}
			if got := starter.count(); got != 1 {
				t.Fatalf("run starts after permanent auth failure = %d, want 1", got)
			}
		})
	}
}

type runnerCredentialStarter struct {
	r       *runner.Runner
	machine *workflow.Machine
}

func (s runnerCredentialStarter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	result, err := s.r.Start(ctx, runner.StartInput{
		RunID:   req.RunID,
		Machine: s.machine,
		Gaggle:  req.Gaggle,
		Trigger: req.Trigger,
		RepoRef: req.RepoRef,
	})
	return StartResult{
		Phase:          result.Phase,
		FinalState:     result.FinalState,
		FailureStage:   result.FailureStage,
		FailureCode:    result.FailureCode,
		FailureMessage: result.FailureMessage,
	}, err
}

func TestCredentialMaterializationFailureOpensRunCircuit(t *testing.T) {
	const (
		capability = "github:issues:write"
		envVar     = "GOOBERS_TEST_CONDITIONAL_CREDENTIAL_UNSET"
	)
	t.Setenv(envVar, "")
	if err := os.Unsetenv(envVar); err != nil {
		t.Fatal(err)
	}

	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "issues", Env: envVar}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	worktrees, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	localRunner, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, []credentials.Grant{{
				Capability: capability,
				Ref:        "issues",
			}}, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewShellExecutor(injector, rec)
		},
		Worktrees:  worktrees,
		RunsDir:    filepath.Join(root, "runs"),
		ScratchDir: filepath.Join(root, "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "conditional-credential",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers-site",
			Start:  "terminal",
			Tasks: []apiv1.Task{{
				Name:         "terminal",
				Type:         apiv1.TaskDeterministic,
				Capabilities: []string{capability},
				Run: &apiv1.DeterministicRun{
					Command:   []string{"true"},
					Workspace: apiv1.WorkspaceScratch,
				},
				Next: workflow.TerminalComplete,
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	identity := WorkflowIdentity{Gaggle: "goobers-site", Workflow: "conditional-credential"}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow: identity.Workflow,
		Gaggle:   identity.Gaggle,
		Starter:  runnerCredentialStarter{r: localRunner, machine: machine},
	}})
	if _, err := sched.Trigger(context.Background(), identity.Workflow, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, func() int {
		if sched.authCircuitOpen(identity) {
			return 1
		}
		return 0
	}, 1)

	_, err = sched.Trigger(context.Background(), identity.Workflow, time.Now())
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("second trigger error = %v, want TriggerRejectedError", err)
	}
	if !strings.HasPrefix(rejected.Reason, ReasonProviderAuth) {
		t.Fatalf("second trigger reason = %q, want %q prefix", rejected.Reason, ReasonProviderAuth)
	}
}
