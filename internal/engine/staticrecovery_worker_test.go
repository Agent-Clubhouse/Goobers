package engine

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
)

// The workflow worker stays alive while the dispatch worker actually stops and
// is replaced. Custody crosses the recorded activity boundary; no branch or
// scheduler is restarted. The activity's pod implementation remains a fixture.
func TestStaticBranchDispatchWorkerReplacementReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	const rootQueue, dispatchQueue = "static-worker-root", "static-worker-dispatch"
	in := staticSchedulerInput{Run: branchBodyInput("dispatch-worker-replacement", false, false).Run}
	in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
	in.Run.Placements[0].Queue = dispatchQueue
	writer, dir := newLiveWriter(t)
	sink := &siblingSettlementEmitter{JournalEmitter: writer, settled: make(chan struct{})}
	ready, draining := make(chan struct{}, 2), make(chan struct{})
	var joined atomic.Bool
	acts := &Activities{Journal: sink, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		stage := stageOfTaskID(env.TaskID)
		if stage == "beta" {
			ready <- struct{}{}
			select {
			case <-draining:
			case <-ctx.Done():
				return apiv1.ResultEnvelope{}, ctx.Err()
			}
		}
		if stage == "join" {
			joined.Store(true)
		}
		return recoveryResult(stage), nil
	}}}
	root := temporalworker.New(server.Client(), rootQueue, temporalworker.Options{})
	root.RegisterWorkflow(staticScheduler)
	root.RegisterActivity(acts)
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	defer root.Stop()
	var first DispatchStageInput
	var oldCalls, newCalls atomic.Int32
	old := temporalworker.New(server.Client(), dispatchQueue, temporalworker.Options{Identity: "before-drain", WorkerStopTimeout: 5 * time.Second})
	old.RegisterActivity(acts)
	old.RegisterActivityWithOptions(func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		oldCalls.Add(1)
		first = input
		ready <- struct{}{}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			activity.RecordHeartbeat(ctx)
			select {
			case <-activity.GetWorkerStopChannel(ctx):
				close(draining)
				return DispatchStageResult{}, dispatchWorkerHandoff(recoveryCustody())
			case <-ctx.Done():
				return DispatchStageResult{}, ctx.Err()
			case <-ticker.C:
			}
		}
	}, activity.RegisterOptions{Name: ActDispatchStage, DisableAlreadyRegisteredCheck: true})
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	stopOld := sync.OnceFunc(old.Stop)
	defer stopOld()
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.Run.RunID, TaskQueue: rootQueue}, staticScheduler, in)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		awaitRecoverySignal(t, ctx, ready)
	}
	stopOld()
	// The sibling can settle while no dispatch worker is available; the join
	// cannot start until the replacement returns the held branch's result.
	awaitRecoverySignal(t, ctx, sink.settled)
	if joined.Load() {
		t.Fatal("join started while dispatch worker was absent")
	}
	replacement := temporalworker.New(server.Client(), dispatchQueue, temporalworker.Options{Identity: "after-drain"})
	replacement.RegisterActivity(acts)
	replacement.RegisterActivityWithOptions(func(_ context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		newCalls.Add(1)
		if input.Custody == nil || *input.Custody != recoveryCustody() || input.Envelope.Attempt != first.Envelope.Attempt || input.Envelope.TaskID != first.Envelope.TaskID || input.Class != first.Class {
			t.Error("replacement changed dispatch identity or lost custody")
		}
		return DispatchStageResult{ResultEnvelope: recoveryResult("alpha")}, nil
	}, activity.RegisterOptions{Name: ActDispatchStage, DisableAlreadyRegisteredCheck: true})
	if err := replacement.Start(); err != nil {
		t.Fatal(err)
	}
	defer replacement.Stop()
	var out staticSchedulerResult
	if err := run.Get(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "" || out.Result.Status != StatusCompleted || oldCalls.Load() != 1 || newCalls.Load() != 1 || !joined.Load() {
		t.Fatalf("status=%s error=%s old=%d replacement=%d joined=%t", out.Result.Status, out.Error, oldCalls.Load(), newCalls.Load(), joined.Load())
	}
	assertRecoveryBarrier(t, liveEvents(t, dir, in.Run.RunID))
	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	identities := map[string]bool{}
	for i, event := range history.Events {
		if started := event.GetActivityTaskStartedEventAttributes(); started != nil {
			identities[started.GetIdentity()] = true
		}
		if event.GetWorkflowTaskStartedEventAttributes() != nil {
			replayStaticRecovery(t, &historypb.History{Events: history.Events[:i+1]})
		}
	}
	if !identities["before-drain"] || !identities["after-drain"] {
		t.Fatalf("history did not record both dispatch workers: %v", identities)
	}
	replayStaticRecovery(t, history)
}
