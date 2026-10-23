package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticSchedulerCancellationDrainsStartedBodies(t *testing.T) {
	in := schedulerInput("cancel-scheduler", 2)
	writer, dir := newLiveWriter(t)
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		if stageOfTaskID(env.TaskID) == "seed" {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "frozen-root"}}, nil
		}
		<-ctx.Done()
		return apiv1.ResultEnvelope{}, ctx.Err()
	}}}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	started := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name != ActRunDeterministic {
			return
		}
		var inv apiv1.InvocationEnvelope
		if err := args.Get(&inv); err != nil {
			t.Fatal(err)
		}
		if stageOfTaskID(inv.TaskID) == "seed" {
			return
		}
		started++
		if started == 2 {
			env.CancelWorkflow()
		}
	})
	env.ExecuteWorkflow(staticScheduler, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if started != 2 || !strings.Contains(out.Error, "canceled") {
		t.Fatalf("started=%d result=%+v", started, out)
	}
	events := liveEvents(t, dir, in.Run.RunID)
	assertSchedulerCompleteness(t, events, []journal.BranchStatus{journal.BranchCancelled, journal.BranchCancelled, journal.BranchCancelled}, "")
	for _, event := range events {
		if event.Type == journal.EventStageStarted && (event.Stage == "gamma" || event.Stage == "join") {
			t.Fatalf("dispatched after cancellation: %+v", event)
		}
	}
}

type schedulerStartEmitter struct {
	writer *livejournal.Writer
	mu     sync.Mutex
	failed bool
}

func (e *schedulerStartEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.mu.Lock()
	fail := false
	for _, op := range req.Ops {
		if !e.failed && op.Event != nil && op.Event.Type == journal.EventBranchStarted {
			e.failed, fail = true, true
		}
	}
	e.mu.Unlock()
	resp, err := e.writer.Emit(ctx, req)
	if fail && err == nil {
		return livejournal.EmitResponse{}, nil // Durable append, incomplete acknowledgement.
	}
	return resp, err
}

func TestStaticSchedulerUncertainStartNeverDispatches(t *testing.T) {
	in := schedulerInput("uncertain-scheduler", 2)
	writer, dir := newLiveWriter(t)
	exec := newScriptedExec(map[string][]scriptedCall{"seed": {succeed(map[string]interface{}{"value": "frozen-root"})}})
	acts := &Activities{Journal: &schedulerStartEmitter{writer: writer}, Workspaces: testWorkspaces(t), Det: exec}
	out := executeScheduler(t, in, acts)
	if !strings.Contains(out.Error, "incomplete owned journal acknowledgement") {
		t.Fatalf("result=%+v error=%s", out.Result, out.Error)
	}
	if len(exec.calls) != 1 || exec.calls["seed"] != 1 {
		t.Fatalf("uncertain start dispatched: %+v", exec.calls)
	}
	events := liveEvents(t, dir, in.Run.RunID)
	assertSchedulerCompleteness(t, events, []journal.BranchStatus{journal.BranchCancelled, journal.BranchCancelled, journal.BranchCancelled}, "")
	starts := 0
	for _, event := range events {
		if event.Type == journal.EventBranchStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("durable start duplicated by reconciliation: %d", starts)
	}
}

func TestStaticSchedulerFailFastCancelsActiveAndQueued(t *testing.T) {
	in := schedulerInput("fail-fast-active", 2)
	in.Run.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	in.Run.Spec.Parallels[0].OnFailure = wf.TargetAbort
	writer, dir := newLiveWriter(t)
	betaStarted := make(chan struct{})
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		switch stageOfTaskID(env.TaskID) {
		case "alpha":
			select {
			case <-betaStarted:
			case <-ctx.Done():
				return apiv1.ResultEnvelope{}, ctx.Err()
			}
			return apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, nil
		case "beta":
			close(betaStarted)
			<-ctx.Done()
			return apiv1.ResultEnvelope{}, ctx.Err()
		case "seed":
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "frozen-root"}}, nil
		default:
			return apiv1.ResultEnvelope{}, fmt.Errorf("unexpected dispatch %s", env.TaskID)
		}
	}}}
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != StatusBlocked {
		t.Fatalf("result=%+v error=%s", out.Result, out.Error)
	}
	assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), []journal.BranchStatus{journal.BranchFailed, journal.BranchCancelled, journal.BranchCancelled}, wf.TargetAbort)
}
