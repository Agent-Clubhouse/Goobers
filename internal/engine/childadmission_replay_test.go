package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/goobers/goobers/internal/gate"
)

// replayChildEntry replays one history under the protocol entry, as a worker
// that carries this slice would after a rolling upgrade. The original
// execution is required: admission receipts bind the parent's Temporal run.
func replayChildEntry(workflowID string) func(*testing.T, *historypb.History) {
	return func(t *testing.T, history *historypb.History) {
		t.Helper()
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
		if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: originalExecution(workflowID, history)}); err != nil {
			t.Fatalf("replay: %v", err)
		}
	}
}

func originalExecution(workflowID string, history *historypb.History) workflow.Execution {
	return workflow.Execution{ID: workflowID, RunID: history.Events[0].GetWorkflowExecutionStartedEventAttributes().GetOriginalExecutionRunId()}
}

// replayEveryWorkflowTask replays the whole history and every prefix ending at
// a started workflow task, which covers a worker replaced at each boundary.
func replayEveryWorkflowTask(t *testing.T, history *historypb.History, replay func(*testing.T, *historypb.History)) int {
	t.Helper()
	t.Run("full", func(t *testing.T) { replay(t, history) })
	boundaries := 0
	for i, event := range history.Events {
		if event.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			boundaries++
			t.Run(fmt.Sprintf("workflow-task-%d", event.GetEventId()), func(t *testing.T) {
				replay(t, &historypb.History{Events: history.Events[:i+1]})
			})
		}
	}
	return boundaries
}

func loadHistoryFixture(t *testing.T, path string) *historypb.History {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var history historypb.History
	if err := protojson.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	return &history
}

func TestEngineChildFamilyRealHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	f := newChildFixture()
	f.parkAckLoss, f.completeAckLoss = true, true
	writer, _ := newLiveWriter(t)
	w := temporalworker.New(c, "child-family-replay", temporalworker.Options{Identity: "fixture-worker"})
	registerChildProtocol(w, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	in := childParentInput()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.RunID, TaskQueue: "child-family-replay"}, "Run", in)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	markers, starts, childID, childRunID := map[string]int{}, 0, "", ""
	for _, event := range history.Events {
		if a := event.GetMarkerRecordedEventAttributes(); a != nil && a.MarkerName == "Version" {
			markers[string(a.Details["change-id"].GetPayloads()[0].GetData())]++
		}
		if a := event.GetStartChildWorkflowExecutionInitiatedEventAttributes(); a != nil {
			starts++
			childID = a.WorkflowId
			if a.WorkflowType.Name != "Run" || a.ParentClosePolicy != enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL || a.WorkflowIdReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE || a.RetryPolicy.GetMaximumAttempts() != 1 {
				t.Fatalf("unsafe child start options: %+v", a)
			}
		}
		if a := event.GetChildWorkflowExecutionStartedEventAttributes(); a != nil {
			childRunID = a.WorkflowExecution.RunId
		}
	}
	if starts != 1 || markers[`"`+childWorkflowChangeID+`"`] != 1 || markers[`"`+childExecutionChangeID+`"`] != 1 {
		t.Fatalf("unexpected protocol commands starts=%d markers=%v", starts, markers)
	}
	// Prefixes cover admission, parking (with a lost acknowledgement), child
	// start, terminal return, continuation and the continued attempt.
	if boundaries := replayEveryWorkflowTask(t, history, replayChildEntry(run.GetID())); boundaries < 8 {
		t.Fatalf("history has too few workflow-task boundaries: %d", boundaries)
	}
	child := hitlFetchHistory(t, ctx, c, childID, childRunID)
	t.Run("child", func(t *testing.T) {
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
		if err := replayer.ReplayWorkflowHistoryWithOptions(nil, child, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: workflow.Execution{ID: childID, RunID: childRunID}}); err != nil {
			t.Fatal(err)
		}
	})
}

// Captured from the HA-P4-08 protocol (#7864, 952c56873) on a real server:
// admission followed by the fence, with no execution marker. A worker that
// carries execution must take the default branch and reproduce the fence.
func TestEngineChildAdmissionOnlyHistoryReplays(t *testing.T) {
	history := loadHistoryFixture(t, "testdata/child-admission-p408.json")
	replayEveryWorkflowTask(t, history, replayChildEntry("run-child-parent"))
}

// The N-1 fixture is a production Run refusing a child parent, captured on
// 9329d721f before this protocol existed. It has no version marker; the new
// entry must take the default branch and reproduce the refusal.
func TestEngineChildPreProtocolRefusalHistoryReplays(t *testing.T) {
	history := loadHistoryFixture(t, "testdata/child-parent-refusal-n1.json")
	for _, event := range history.Events {
		if a := event.GetMarkerRecordedEventAttributes(); a != nil {
			t.Fatalf("N-1 fixture unexpectedly has marker %q", a.MarkerName)
		}
	}
	replayEveryWorkflowTask(t, history, replayChildEntry("child-parent-n1"))
	replayEveryWorkflowTask(t, history, func(t *testing.T, h *historypb.History) {
		t.Helper()
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(Run)
		if err := replayer.ReplayWorkflowHistoryWithOptions(nil, h, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: originalExecution("child-parent-n1", h)}); err != nil {
			t.Fatalf("production replay: %v", err)
		}
	})
}

// Ordinary saved histories must not see a child command on the protocol entry.
func TestEngineChildEntryReplaysPreChangeOrdinaryHistories(t *testing.T) {
	for _, file := range []string{"testdata/dispatchstage_history.json", "testdata/learning-episode-prechange-history.json", "testdata/continuity-prechange-history.json"} {
		t.Run(file, func(t *testing.T) {
			r := temporalworker.NewWorkflowReplayer()
			r.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, file); err != nil {
				t.Fatal(err)
			}
		})
	}
}
