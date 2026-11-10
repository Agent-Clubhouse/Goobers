package intervention

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginedecision"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workerhost"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// Preserve the review probe's API-replica barrier, but route every query and
// Update to the real workflow. A fake that accepts all gate Updates cannot
// enforce the workflow's shared request-key namespace.
type terminalRouteBarrier struct {
	enginedecision.Router
	blocked  atomic.Bool
	queried  chan struct{}
	resume   chan struct{}
	snapshot engine.HITLState
}

func (r *terminalRouteBarrier) HITLState(ctx context.Context, run string, ids ...string) (engine.HITLState, error) {
	snapshot, err := r.Router.HITLState(ctx, run, ids...)
	if err == nil && len(ids) > 0 && r.blocked.CompareAndSwap(false, true) {
		r.snapshot = snapshot
		close(r.queried)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return engine.HITLState{}, ctx.Err()
		}
	}
	return snapshot, err
}

func waitTerminalRouteState(t *testing.T, ctx context.Context, ready func() (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := ready()
		if err == nil && ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for workflow state: %v (last query: %v)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func terminalRouteWorkflow(t *testing.T, ctx context.Context) (client.Client, client.WorkflowRun, *livejournal.Writer) {
	t.Helper()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	repository := localFixtureRepo(t)
	worker := temporalworker.New(server.Client(), "terminal-route-barrier", temporalworker.Options{})
	engine.RegisterWith(worker, &engine.Activities{Journal: writer, Det: succeedingDeterministic{}, Workspaces: &workerhost.WorktreeWorkspaces{Manager: manager, ScratchDir: t.TempDir(), CloneURL: func(apiv1.RepoRef) (string, error) { return repository, nil }}})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	preview := true
	input := engine.RunInput{RunID: "engine-run", Gaggle: "example", WorkflowName: "release", Version: 1, DSLVersion: "3.1", PreviewFeaturesEnabled: &preview, TriggerKind: string(journal.TriggerManual), LiveJournal: true,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"}, HITL: &engine.HITLPolicy{Enabled: true, WaitSeconds: 300, Actors: []string{"alice"}},
		Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "review", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Tasks: []apiv1.Task{{Name: "implement", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review"}},
			Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{Approvers: []string{"alice"}}, Branches: map[string]string{"pass": "implement", "reject": wf.TargetEscalate}}}}}
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: input.RunID, TaskQueue: "terminal-route-barrier"}, engine.Run, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Client().CancelWorkflow(context.Background(), run.GetID(), run.GetRunID()) })
	return server.Client(), run, writer
}

func TestTerminalRetryAcrossLookupCannotApproveLaterHumanGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c, run, _ := terminalRouteWorkflow(t, ctx)
	router := enginedecision.Router{Client: c}
	var initial engine.GateDecisionState
	waitTerminalRouteState(t, ctx, func() (bool, error) {
		var err error
		initial, err = router.GateState(ctx, run.GetID())
		return initial.Pending, err
	})
	seed := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: run.GetID(), RequestID: "initial-gate-decision", Actor: "alice", Gate: "review", GateOccurrence: initial.GateOccurrence, ExpectedTerminalGeneration: initial.TerminalGeneration, Decision: "reject"}
	if _, err := router.DecideGate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	waitTerminalRouteState(t, ctx, func() (bool, error) {
		state, err := router.HITLState(ctx, run.GetID())
		return state.Phase == "awaiting-operator", err
	})
	barrier := &terminalRouteBarrier{Router: router, queried: make(chan struct{}), resume: make(chan struct{})}
	service := engineRunService(t, barrier)
	request := httpapi.InterventionRequest{RunID: run.GetID(), Stage: "review", Actor: "alice", IdempotencyKey: "same-approval", Decision: "pass"}
	duplicate := make(chan error, 1)
	go func() { _, err := service.Approve(ctx, request); duplicate <- err }()
	select {
	case <-barrier.queried:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if barrier.snapshot.RequestKnown {
		t.Fatal("premise: duplicate lookup already knew the request")
	}
	if _, err := service.Approve(ctx, request); err != nil {
		t.Fatalf("original terminal approval: %v", err)
	}
	var later engine.GateDecisionState
	waitTerminalRouteState(t, ctx, func() (bool, error) {
		var err error
		later, err = router.GateState(ctx, run.GetID())
		return later.Pending && later.GateOccurrence > initial.GateOccurrence, err
	})
	close(barrier.resume)
	var err error
	select {
	case err = <-duplicate:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var refusal *httpapi.InterventionError
	if !errors.As(err, &refusal) || refusal.Code != engine.GateDecisionErrKeyReused {
		t.Fatalf("duplicate crossed protocol after stale lookup: %v", err)
	}
	after, err := router.GateState(ctx, run.GetID())
	if err != nil || !after.Pending || after.GateOccurrence != later.GateOccurrence {
		t.Fatalf("later gate was decided: before=%+v after=%+v error=%v", later, after, err)
	}
}
