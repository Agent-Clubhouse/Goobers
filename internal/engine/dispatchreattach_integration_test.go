package engine

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func reattachReplayWorkflow(ctx workflow.Context) error {
	ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "")
	_, err := executeDispatchActivity(ctx, dispatchInput("reattach-replay", "build", 1))
	return err
}

func reattachReplayDevServer(t *testing.T) (context.Context, *testsuite.DevServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = server.Stop() })
	return ctx, server
}

func TestIntegrationDispatchReattachMarkedHandoffHistoryReplays(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, server := reattachReplayDevServer(t)
	queue := "dispatch-reattach-replay"
	var calls atomic.Int32
	w := worker.New(server.Client(), queue, worker.Options{})
	w.RegisterWorkflow(reattachReplayWorkflow)
	w.RegisterActivityWithOptions(func(context.Context, DispatchStageInput) (DispatchStageResult, error) {
		if calls.Add(1) == 1 {
			return DispatchStageResult{}, dispatchWorkerHandoff(dispatcher.PodCustody{
				Namespace: "web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true,
			})
		}
		return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	}, activity.RegisterOptions{Name: ActDispatchStage})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "reattach-replay", TaskQueue: queue}, reattachReplayWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	history := &historypb.History{}
	iter := server.Client().GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	hasMarker, hasHandoff := false, false
	for _, event := range history.Events {
		if marker := event.GetMarkerRecordedEventAttributes(); marker != nil && marker.GetMarkerName() == "Version" {
			var changeID string
			if payloads := marker.GetDetails()["change-id"]; payloads != nil &&
				converter.GetDefaultDataConverter().FromPayloads(payloads, &changeID) == nil &&
				changeID == dispatchReattachChange {
				hasMarker = true
			}
		}
		if failed := event.GetActivityTaskFailedEventAttributes(); failed != nil &&
			failureHasType(failed.GetFailure(), failureTypeWorkerHandoff) {
			hasHandoff = true
		}
	}
	if !hasMarker || !hasHandoff {
		t.Fatal("captured history lacks reattach marker or handoff failure")
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(reattachReplayWorkflow)
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, worker.ReplayWorkflowHistoryOptions{
		OriginalExecution: workflow.Execution{ID: run.GetID(), RunID: run.GetRunID()},
	}); err != nil {
		t.Fatalf("replay marked handoff history: %v", err)
	}
}

func failureHasType(failure *failurepb.Failure, failureType string) bool {
	for current := failure; current != nil; current = current.GetCause() {
		if current.GetApplicationFailureInfo().GetType() == failureType {
			return true
		}
	}
	return false
}
