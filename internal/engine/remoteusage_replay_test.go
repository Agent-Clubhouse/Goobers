package engine

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/usageallowance"
)

func TestRemoteUsageAllowanceHistoryAndPrefixesReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})

	const queue = "remote-usage-replay"
	input := dispatchInput("remote-usage-replay", "implement", 1)
	input.Placement.Queue = queue
	store := surrenderStore(t)
	putSurrendered(t, store, input.Envelope.RunID, "implement", 1, dispatcher.SurrenderedResult{
		ContractVersion: dispatcher.CurrentSurrenderContract,
		ObservedUsage:   usageMetrics(7, 0.2), ObservedUsageReported: true,
		Result: apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Metrics: usageMetrics(999, 999),
		},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "linux", Pod: "usage-pod", Phase: corev1.PodSucceeded,
		SurrenderConfirmed: true, Disposed: true,
	}}
	worker := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	worker.RegisterWorkflow(remoteUsageReplayWorkflow)
	worker.RegisterActivity(&Activities{Dispatcher: fake, Surrenders: store})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)

	tokens, cost := int64(10), 0.3
	allowance := usageallowance.Allowance{MaxTokens: &tokens, MaxCostUSD: &cost}
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "remote-usage-replay", TaskQueue: queue,
	}, remoteUsageReplayWorkflow, input, allowance)
	if err != nil {
		t.Fatal(err)
	}
	var usage AttemptUsage
	if err := run.Get(ctx, &usage); err != nil {
		t.Fatal(err)
	}
	if !usage.Reported || usage.Metrics["gen_ai.usage.input_tokens"] != 7 {
		t.Fatalf("workflow usage = %+v", usage)
	}
	history := readRemoteUsageHistory(t, ctx, server.Client(), run)
	scheduled := scheduledRemoteUsageInput(t, history)
	if scheduled.UsageAllowance == nil || scheduled.UsageAllowance.MaxTokens == nil ||
		*scheduled.UsageAllowance.MaxTokens != 10 {
		t.Fatalf("scheduled activity input allowance = %+v", scheduled.UsageAllowance)
	}
	replayRemoteUsageHistory(t, history, "full")
	prefixes := 0
	for index, event := range history.Events {
		if event.EventType != enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			continue
		}
		prefixes++
		replayRemoteUsageHistory(t, &historypb.History{
			Events: append([]*historypb.HistoryEvent(nil), history.Events[:index+1]...),
		}, "prefix")
	}
	if prefixes < 2 {
		t.Fatalf("workflow-task-start prefixes = %d, want at least two", prefixes)
	}
}

func remoteUsageReplayWorkflow(
	ctx workflow.Context,
	input DispatchStageInput,
	allowance usageallowance.Allowance,
) (AttemptUsage, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	input.UsageAllowance = versionedRemoteUsageAllowance(ctx, &allowance)
	result, err := executeDispatchActivity(ctx, input)
	if err != nil {
		return AttemptUsage{}, err
	}
	if result.Usage == nil {
		return AttemptUsage{}, fmt.Errorf("dispatch returned no trusted usage")
	}
	return *result.Usage, nil
}

func readRemoteUsageHistory(
	t *testing.T,
	ctx context.Context,
	temporalClient client.Client,
	run client.WorkflowRun,
) *historypb.History {
	t.Helper()
	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	return history
}

func scheduledRemoteUsageInput(t *testing.T, history *historypb.History) DispatchStageInput {
	t.Helper()
	for _, event := range history.Events {
		attributes := event.GetActivityTaskScheduledEventAttributes()
		if attributes == nil || attributes.ActivityType.GetName() != ActDispatchStage {
			continue
		}
		var input DispatchStageInput
		if err := converter.GetDefaultDataConverter().FromPayloads(attributes.Input, &input); err != nil {
			t.Fatal(err)
		}
		return input
	}
	t.Fatal("history has no DispatchStage activity")
	return DispatchStageInput{}
}

func replayRemoteUsageHistory(t *testing.T, history *historypb.History, label string) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(remoteUsageReplayWorkflow)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay %s history: %v", label, err)
	}
}
