package engine

import (
	"context"
	"io"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestGateDecisionAcceptedHistoryAndPrefixesReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	writer, _ := newLiveWriter(t)
	const queue = "gate-decision-replay"
	worker := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	worker.RegisterWorkflow(Run)
	worker.RegisterActivity(&Activities{Journal: writer})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)

	in := runInput("gate-decision-replay", repeatedHumanGateSpec())
	in.RunID = "gate-decision-replay"
	in.TriggerKind = string(journal.TriggerManual)
	in.LiveJournal = true
	in.HITL = &HITLPolicy{Enabled: true, WaitSeconds: 60, Actors: []string{"operator"}}
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: in.RunID, TaskQueue: queue,
	}, "Run", in)
	if err != nil {
		t.Fatal(err)
	}
	occurrence := waitForGateDecisionState(t, ctx, server.Client(), run)
	request := engineGateDecisionRequest("replay-decision", occurrence, "needs-changes")
	request.RunID = in.RunID
	handle, err := server.Client().UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: run.GetID(), RunID: run.GetRunID(),
		UpdateID: GateDecisionUpdateID(request), UpdateName: GateDecisionUpdateName,
		Args: []interface{}{request}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ack GateDecisionAck
	if err := handle.Get(ctx, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.GateOccurrence != occurrence || ack.Decision != "needs-changes" {
		t.Fatalf("ack=%+v occurrence=%d", ack, occurrence)
	}
	nextOccurrence := waitForGateDecisionState(t, ctx, server.Client(), run)
	changed := request
	changed.GateOccurrence = nextOccurrence
	changed.Decision = "pass"
	staleHandle, err := server.Client().UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: run.GetID(), RunID: run.GetRunID(),
		UpdateID: GateDecisionUpdateID(request), UpdateName: GateDecisionUpdateName,
		Args: []interface{}{changed}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	var staleAck GateDecisionAck
	if err := staleHandle.Get(ctx, &staleAck); err != nil {
		t.Fatal(err)
	}
	if err := staleAck.ValidateRequest(changed); err == nil {
		t.Fatalf("server-replayed stale ack matched changed request: %+v", staleAck)
	}
	changedHandle, err := server.Client().UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: run.GetID(), RunID: run.GetRunID(),
		UpdateID: GateDecisionUpdateID(changed), UpdateName: GateDecisionUpdateName,
		Args: []interface{}{changed}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	changedErr := changedHandle.Get(ctx, &GateDecisionAck{})
	if code, _, ok := gateDecisionRefusalCode(changedErr); !ok || code != GateDecisionErrKeyReused {
		t.Fatal("changed payload under reused requestId was accepted")
	}
	final := changed
	final.RequestID = "replay-final"
	finalHandle, err := server.Client().UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: run.GetID(), RunID: run.GetRunID(),
		UpdateID: GateDecisionUpdateID(final), UpdateName: GateDecisionUpdateName,
		Args: []interface{}{final}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := finalHandle.Get(ctx, &GateDecisionAck{}); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}

	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	replayGateDecisionHistory(t, history)
	prefixes := 0
	for i, event := range history.Events {
		if event.EventType != enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			continue
		}
		prefixes++
		replayGateDecisionHistory(t, &historypb.History{
			Events: append([]*historypb.HistoryEvent(nil), history.Events[:i+1]...),
		})
	}
	if prefixes < 3 {
		t.Fatalf("history has %d workflow-task prefixes", prefixes)
	}
}

func waitForGateDecisionState(t *testing.T, ctx context.Context, c client.Client, run client.WorkflowRun) uint64 {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), GateDecisionStateQuery)
		if err == nil {
			var state GateDecisionState
			if err := value.Get(&state); err != nil {
				t.Fatal(err)
			}
			if state.Pending {
				return state.GateOccurrence
			}
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
