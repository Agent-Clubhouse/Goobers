package engine

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStaticBranchHandoffRealHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	in := staticSchedulerInput{Run: branchBodyInput("replay-branch-handoff", false, false).Run}
	in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
	writer, dir := newLiveWriter(t)
	sink := &siblingSettlementEmitter{JournalEmitter: writer, settled: make(chan struct{})}
	acts := &Activities{Journal: sink, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return recoveryResult(stageOfTaskID(env.TaskID)), nil
	}}}
	var calls atomic.Int32
	const queue = "static-branch-recovery"
	in.Run.Placements[0].Queue = queue
	w := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	w.RegisterWorkflow(staticScheduler)
	w.RegisterActivity(acts)
	w.RegisterActivityWithOptions(func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		if calls.Add(1) == 1 {
			return DispatchStageResult{}, dispatchWorkerHandoff(dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "alpha", UID: "alpha-uid", Attempt: 1, Custody: true})
		}
		if input.Custody == nil || input.Custody.UID != "alpha-uid" || input.Envelope.Attempt != 1 {
			t.Error("captured recovery changed custody/attempt")
		}
		select {
		case <-sink.settled:
		case <-ctx.Done():
			return DispatchStageResult{}, ctx.Err()
		}
		return DispatchStageResult{ResultEnvelope: recoveryResult("alpha")}, nil
	}, activity.RegisterOptions{Name: ActDispatchStage, DisableAlreadyRegisteredCheck: true})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.Run.RunID, TaskQueue: queue}, staticScheduler, in)
	if err != nil {
		t.Fatal(err)
	}
	var result staticSchedulerResult
	if err := run.Get(ctx, &result); err != nil || result.Error != "" || result.Result.Status != StatusCompleted || calls.Load() != 2 {
		t.Fatalf("recovery result: %+v calls=%d err=%v", result, calls.Load(), err)
	}
	assertRecoveryBarrier(t, liveEvents(t, dir, in.Run.RunID))
	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	marked := false
	for _, event := range history.Events {
		if failed := event.GetActivityTaskFailedEventAttributes(); failed != nil && failureHasType(failed.GetFailure(), failureTypeWorkerHandoff) {
			marked = true
		}
	}
	if !marked {
		t.Fatal("captured history has no recorded worker handoff")
	}
	for i, event := range history.Events {
		if event.GetWorkflowTaskStartedEventAttributes() != nil {
			replayStaticRecovery(t, &historypb.History{Events: history.Events[:i+1]})
		}
	}
	replayStaticRecovery(t, history)
}

func replayStaticRecovery(t *testing.T, history *historypb.History) {
	t.Helper()
	r := temporalworker.NewWorkflowReplayer()
	r.RegisterWorkflow(staticScheduler)
	if err := r.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay at %d events: %v", len(history.Events), err)
	}
}
