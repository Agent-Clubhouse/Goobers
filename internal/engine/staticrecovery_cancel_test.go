package engine

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// A real server delivers cancellation through activity heartbeats. This proves
// activity cancellation and branch settlement, not physical pod-writer shutdown.
func TestStaticBranchCancellationDuringReattachmentRealHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	in := schedulerInput("cancel-reattachment-history", 2)
	const queue = "static-recovery-cancellation"
	in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
	in.Run.Placements[0].Queue = queue
	writer, dir := newLiveWriter(t)
	ready, stopped := make(chan struct{}, 2), make(chan struct{}, 2)
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		if stageOfTaskID(env.TaskID) == "seed" {
			return recoveryResult("seed"), nil
		}
		if stageOfTaskID(env.TaskID) != "beta" {
			t.Errorf("unexpected dispatch %s", env.TaskID)
		}
		return apiv1.ResultEnvelope{}, awaitRecoveryCancellation(ctx, ready, stopped)
	}}}
	var calls atomic.Int32
	w := temporalworker.New(server.Client(), queue, temporalworker.Options{MaxHeartbeatThrottleInterval: 100 * time.Millisecond, DefaultHeartbeatThrottleInterval: 100 * time.Millisecond})
	w.RegisterWorkflow(staticScheduler)
	w.RegisterActivity(acts)
	w.RegisterActivityWithOptions(func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		if calls.Add(1) == 1 {
			return DispatchStageResult{}, dispatchWorkerHandoff(recoveryCustody())
		}
		if input.Custody == nil || *input.Custody != recoveryCustody() || input.Envelope.Attempt != 1 {
			t.Error("reattachment lost custody or attempt")
		}
		return DispatchStageResult{}, awaitRecoveryCancellation(ctx, ready, stopped)
	}, activity.RegisterOptions{Name: ActDispatchStage, DisableAlreadyRegisteredCheck: true})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.Run.RunID, TaskQueue: queue}, staticScheduler, in)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		awaitRecoverySignal(t, ctx, ready)
	}
	if err := server.Client().CancelWorkflow(ctx, run.GetID(), run.GetRunID()); err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := run.Get(ctx, &out); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		awaitRecoverySignal(t, ctx, stopped)
	}
	if calls.Load() != 2 || !strings.Contains(strings.ToLower(out.Error), "canceled") {
		t.Fatalf("calls=%d result=%+v", calls.Load(), out)
	}
	events := liveEvents(t, dir, in.Run.RunID)
	assertSchedulerCompleteness(t, events, []journal.BranchStatus{journal.BranchCancelled, journal.BranchCancelled, journal.BranchCancelled}, "")
	for _, event := range events {
		if event.Type == journal.EventStageStarted && (event.Stage == "gamma" || event.Stage == "join") {
			t.Fatalf("dispatched after cancellation: %+v", event)
		}
	}
	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	for i, event := range history.Events {
		if event.GetWorkflowTaskStartedEventAttributes() != nil {
			replayStaticRecovery(t, &historypb.History{Events: history.Events[:i+1]})
		}
	}
	replayStaticRecovery(t, history)
}

func awaitRecoveryCancellation(ctx context.Context, ready, stopped chan<- struct{}) error {
	ready <- struct{}{}
	defer func() { stopped <- struct{}{} }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		activity.RecordHeartbeat(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func awaitRecoverySignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
