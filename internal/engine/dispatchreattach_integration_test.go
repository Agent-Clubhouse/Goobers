//go:build integration

package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func reattachReplayWorkflow(ctx workflow.Context) error {
	ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "")
	_, err := executeDispatchActivity(ctx, dispatchInput("reattach-replay", "build", 1))
	return err
}

func TestIntegrationDispatchReattachMarkedHandoffHistoryReplays(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, server := cancellationDevServer(t)
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
	encoded, err := protojson.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), dispatchReattachChange) ||
		!strings.Contains(string(encoded), failureTypeWorkerHandoff) {
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
