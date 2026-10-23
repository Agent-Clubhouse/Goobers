package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
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

func TestEngineChildAdmissionRealHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	f := newChildFixture()
	writer, _ := newLiveWriter(t)
	w := temporalworker.New(c, "child-admission-replay", temporalworker.Options{Identity: "fixture-worker"})
	registerChildProtocol(w, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	in := childParentInput()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.RunID, TaskQueue: "child-admission-replay"}, "Run", in)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Get(ctx, nil); err == nil || !strings.Contains(err.Error(), errChildExecutionUnavailable.Error()) {
		t.Fatalf("admission crossed the activation boundary: %v", err)
	}
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	markers, activities := 0, map[string]int{}
	for _, event := range history.Events {
		if a := event.GetMarkerRecordedEventAttributes(); a != nil && a.MarkerName == "Version" &&
			strings.Contains(string(a.Details["change-id"].GetPayloads()[0].GetData()), childWorkflowChangeID) {
			markers++
		}
		if a := event.GetActivityTaskScheduledEventAttributes(); a != nil {
			activities[a.ActivityType.Name]++
		}
		if event.GetStartChildWorkflowExecutionInitiatedEventAttributes() != nil {
			t.Fatal("HA-P4-08 started a child workflow")
		}
	}
	if markers != 1 || activities["AdmitEngineChild"] != 1 || activities["CancelEngineChild"] != 1 || activities[ActInvokeGoober] != 1 {
		t.Fatalf("unexpected protocol commands markers=%d activities=%v", markers, activities)
	}
	if boundaries := replayEveryWorkflowTask(t, history, replayChildEntry(run.GetID())); boundaries < 4 {
		t.Fatalf("history has too few workflow-task boundaries: %d", boundaries)
	}
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
