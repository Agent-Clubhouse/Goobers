package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/gate"
)

func replayECWHistory(t *testing.T, history *historypb.History, id, runID string) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: workflow.Execution{ID: id, RunID: runID}}); err != nil {
		t.Fatalf("replay %s: %v", id, err)
	}
}

func TestEngineChildRealHistoriesReplayAtEveryWaitBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	f := newECWFixture()
	f.parkAckLoss = true
	f.completeAckLoss = true
	writer, _ := newLiveWriter(t)
	w := temporalworker.New(c, "ecw-replay", temporalworker.Options{})
	registerECW(w, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	in := ecwInput()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.RunID, TaskQueue: "ecw-replay"}, "Run", in)
	if err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err = run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	replayECWHistory(t, history, run.GetID(), run.GetRunID())
	boundaries, starts, markers := 0, 0, 0
	// Replay prefixes at server workflow-task boundaries. This covers recorded
	// admission, parking, child start, terminal return and continuation, including
	// worker replacement while the next effect has not acknowledged yet.
	for i, event := range history.Events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED:
			boundaries++
			t.Run(fmt.Sprintf("boundary-%d", event.EventId), func(t *testing.T) {
				replayECWHistory(t, &historypb.History{Events: history.Events[:i+1]}, run.GetID(), run.GetRunID())
			})
		case enumspb.EVENT_TYPE_START_CHILD_WORKFLOW_EXECUTION_INITIATED:
			starts++
			a := event.GetStartChildWorkflowExecutionInitiatedEventAttributes()
			if a.WorkflowType.Name != "Run" || a.ParentClosePolicy != enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL || a.WorkflowIdReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
				t.Fatalf("unsafe child start options: %+v", a)
			}
		case enumspb.EVENT_TYPE_MARKER_RECORDED:
			if event.GetMarkerRecordedEventAttributes().MarkerName == "Version" {
				markers++
			}
		}
	}
	if starts != 1 || markers == 0 || boundaries < 8 {
		t.Fatalf("missing protocol history starts=%d markers=%d boundaries=%d", starts, markers, boundaries)
	}
	f.mu.Lock()
	var binding ChildRunBinding
	for _, b := range f.bindings {
		binding = b
	}
	f.mu.Unlock()
	childHistory := hitlFetchHistory(t, ctx, c, childWorkflowID(binding), "")
	childRunID := ""
	for _, event := range history.Events {
		if a := event.GetChildWorkflowExecutionStartedEventAttributes(); a != nil {
			childRunID = a.WorkflowExecution.RunId
		}
	}
	replayECWHistory(t, childHistory, childWorkflowID(binding), childRunID)
}

func TestEngineChildLegacyRefusalHistoryReplaysWithoutMarker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	w := temporalworker.New(c, "ecw-legacy", temporalworker.Options{})
	w.RegisterWorkflow(Run)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	in := ecwInput()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "ecw-legacy", TaskQueue: "ecw-legacy", RetryPolicy: nil}, Run, in)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Get(ctx, nil); err == nil {
		t.Fatal("legacy run did not refuse children")
	}
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	replayECWHistory(t, history, run.GetID(), run.GetRunID())
}

func TestEngineChildOldDispatchAndLearningHistoriesReplay(t *testing.T) {
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

func TestEngineChildRealFamilyCancellationCustody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	for _, terminate := range []bool{false, true} {
		t.Run(fmt.Sprint(terminate), func(t *testing.T) {
			f := newECWFixture()
			f.blockChild = true
			writer, _ := newLiveWriter(t)
			queue := fmt.Sprintf("ecw-cancel-%v", terminate)
			w := temporalworker.New(c, queue, temporalworker.Options{MaxHeartbeatThrottleInterval: 10 * time.Millisecond, DefaultHeartbeatThrottleInterval: 10 * time.Millisecond})
			registerECW(w, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			in := ecwInput()
			in.RunID = queue
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.RunID, TaskQueue: queue}, "Run", in)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-f.childStarted:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			f.mu.Lock()
			var binding ChildRunBinding
			for _, b := range f.bindings {
				binding = b
			}
			f.mu.Unlock()
			if terminate {
				err = c.TerminateWorkflow(ctx, run.GetID(), run.GetRunID(), "test parent-close cancellation")
			} else {
				err = c.CancelWorkflow(ctx, run.GetID(), run.GetRunID())
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = run.Get(ctx, nil); err == nil {
				t.Fatal("cancelled parent succeeded")
			}
			child := c.GetWorkflow(ctx, childWorkflowID(binding), "")
			var result RunResult
			if err = child.Get(ctx, &result); err != nil {
				t.Fatal(err)
			}
			if result.ChildResult == nil || result.ChildResult.Input.State != "cancelled" {
				t.Fatalf("child cancellation lost retained custody: %+v", result)
			}
			f.mu.Lock()
			if f.parentSlots != 0 || f.childSlots != 0 || len(f.envelopes) != 1 || (!terminate && !f.cancelObserved) {
				t.Errorf("family cancellation resumed parent: %v", f.calls)
			}
			f.mu.Unlock()
			if !terminate {
				replayECWHistory(t, hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID()), run.GetID(), run.GetRunID())
			}
			replayECWHistory(t, hitlFetchHistory(t, ctx, c, child.GetID(), child.GetRunID()), child.GetID(), child.GetRunID())
		})
	}
}
