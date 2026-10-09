package runner

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type pendingChildExecutor struct {
	calls   atomic.Int32
	entered chan struct{}
	proceed chan struct{}
}

func (p *pendingChildExecutor) pending() error {
	p.calls.Add(1)
	if p.entered != nil {
		close(p.entered)
		<-p.proceed
	}
	return invoke.ErrChildCustodyPending
}
func (p *pendingChildExecutor) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, p.pending()
}
func (p *pendingChildExecutor) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, p.pending()
}
func (p *pendingChildExecutor) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, p.pending()
}

func pendingChildRunner(t *testing.T, kind string, executor *pendingChildExecutor) (*Runner, StartInput) {
	t.Helper()
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := New(Config{Worktrees: manager, RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"), ConfigGeneration: journal.Digest([]byte("config"))})
	if err != nil {
		t.Fatal(err)
	}
	definition := childWorkspaceMachine(t, apiv1.WorkspaceScratch, false).Def
	if kind == "deterministic" {
		definition.Spec.Tasks[0].Type = apiv1.TaskDeterministic
		definition.Spec.Tasks[0].Goober = ""
		definition.Spec.Tasks[0].Goal = ""
		definition.Spec.Tasks[0].Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}
	}
	if kind == "reviewer" {
		definition.Spec.Start = "review"
		definition.Spec.Gates = []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "coder", Workspace: apiv1.WorkspaceScratch, Retry: &apiv1.RetryPolicy{MaxAttempts: 3}}, Branches: map[string]string{"pass": "work", "needs-changes": "work", "fail": workflow.TargetAbort}}}
	}
	machine, err := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	in := childWorkspaceStart(machine)
	id := journal.RunIdentity{RunID: in.RunID, Gaggle: in.Gaggle, Workflow: machine.Def.Name, WorkflowDigest: machine.Digest(), GooberDigest: in.GooberDigest, ConfigGeneration: base.cfg.ConfigGeneration, Child: in.Child}
	isolated, err := base.ForChildExecution(id, ChildExecutionFactories{
		NewAgentic:            func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return executor, nil },
		NewDeterministic:      func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return executor, nil },
		VerifyTerminalCustody: func(*journal.Run) error { return invoke.ErrChildCustodyPending },
	})
	if err != nil {
		t.Fatal(err)
	}
	return isolated, in
}

func assertChildPendingHistory(t *testing.T, r *Runner, in StartInput) {
	t.Helper()
	rd, err := journal.OpenReadOnly(filepath.Join(r.cfg.RunsDir, in.RunID))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := rd.Phase()
	if err != nil || phase != journal.PhaseRunning {
		t.Fatal("pending custody claimed terminal", phase, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunFinished || event.Type == journal.EventStageFinished || event.Type == journal.EventReviewerFinished || event.Type == journal.EventGateEvaluated {
			t.Fatal("pending custody manufactured an outcome", event)
		}
	}
}

func TestChildPendingCustodyDoesNotRetryOrFinishAnyExecutorKind(t *testing.T) {
	for _, kind := range []string{"agentic", "deterministic", "reviewer"} {
		t.Run(kind, func(t *testing.T) {
			executor := &pendingChildExecutor{}
			r, in := pendingChildRunner(t, kind, executor)
			result, err := r.Start(t.Context(), in)
			if result.Phase != journal.PhaseRunning || !errors.Is(err, invoke.ErrChildCustodyPending) || executor.calls.Load() != 1 {
				t.Fatal("pending attempt retried or settled", result, err, executor.calls.Load())
			}
			assertChildPendingHistory(t, r, in)
		})
	}
}

func TestChildCancellationGraceCannotDetachUnjoinedOwner(t *testing.T) {
	executor := &pendingChildExecutor{entered: make(chan struct{}), proceed: make(chan struct{})}
	r, in := pendingChildRunner(t, "agentic", executor)
	r.stalledCancelGrace = time.Millisecond
	done := make(chan error, 1)
	go func() { _, err := r.Start(t.Context(), in); done <- err }()
	select {
	case <-executor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child never entered")
	}
	result, terminal, err := r.CancelRun(in.RunID, time.Now())
	if terminal || result.Phase != journal.PhaseRunning || !errors.Is(err, invoke.ErrChildCustodyPending) {
		t.Fatal("cancellation claimed worker stopped", result, terminal, err)
	}
	assertChildPendingHistory(t, r, in)
	select {
	case err := <-done:
		t.Fatal("owner detached before join", err)
	default:
	}
	close(executor.proceed)
	select {
	case err := <-done:
		if !errors.Is(err, invoke.ErrChildCustodyPending) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not return after join attempt")
	}
	assertChildPendingHistory(t, r, in)
}
