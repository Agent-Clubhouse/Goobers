package engine

// Worker-loss qualification for generated-child families on a real Temporal
// server (HA-P4-10). Workers are actually stopped and replaced; activity pod
// implementations remain fixtures. The nightly kind lane (CHAOS-13) runs the
// same shapes against real pods once the chaos harness lands.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
)

const childDispatchQueue = "child-recovery-dispatch"

// childRecoveryHarness hosts the family's workflows and custody activities on
// one queue and the child's only stage on a separately replaceable queue.
type childRecoveryHarness struct {
	t        *testing.T
	ctx      context.Context
	c        client.Client
	f        *childFixture
	acts     *Activities
	dir      string
	in       RunInput
	rootName string
}

func newChildRecoveryHarness(t *testing.T, ctx context.Context, c client.Client, name string) *childRecoveryHarness {
	t.Helper()
	f := newChildFixture()
	f.changeLoad = func(m *FrozenChildManifest) {
		pin := remotePin("work")
		pin.Queue = childDispatchQueue + "-" + name
		m.Input.Placements = []PinnedPlacement{pin}
	}
	writer, dir := newLiveWriter(t)
	in := childParentInput()
	in.RunID = "run-" + name
	return &childRecoveryHarness{t: t, ctx: ctx, c: c, f: f, dir: dir, in: in, rootName: "child-recovery-root-" + name,
		acts: &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}}
}

func (h *childRecoveryHarness) startRoot(identity string) temporalworker.Worker {
	h.t.Helper()
	w := temporalworker.New(h.c, h.rootName, temporalworker.Options{Identity: identity})
	registerChildProtocol(w, h.acts, h.f)
	if err := w.Start(); err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *childRecoveryHarness) startDispatch(identity, queue string, fn func(context.Context, DispatchStageInput) (DispatchStageResult, error)) temporalworker.Worker {
	h.t.Helper()
	w := temporalworker.New(h.c, queue, temporalworker.Options{Identity: identity, WorkerStopTimeout: 5 * time.Second})
	// Dispatch workers serve the full activity set, as in production; the
	// stage's journal emission runs on its dispatch queue.
	w.RegisterActivity(h.acts)
	w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: ActDispatchStage, DisableAlreadyRegisteredCheck: true})
	if err := w.Start(); err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *childRecoveryHarness) start() client.WorkflowRun {
	h.t.Helper()
	run, err := h.c.ExecuteWorkflow(h.ctx, client.StartWorkflowOptions{ID: h.in.RunID, TaskQueue: h.rootName}, "Run", h.in)
	if err != nil {
		h.t.Fatal(err)
	}
	return run
}

// await fails with the child's journal instead of blocking the suite when an
// expected boundary is never reached.
func (h *childRecoveryHarness) await(signal <-chan struct{}) {
	h.t.Helper()
	select {
	case <-signal:
	case <-time.After(90 * time.Second):
		var events []string
		for _, b := range h.f.bindings {
			for _, e := range liveEvents(h.t, h.dir, b.Child.RunID) {
				events = append(events, fmt.Sprintf("%s stage=%s attempt=%d class=%s err=%v", e.Type, e.Stage, e.Attempt, e.AttemptClass, e.Error))
			}
		}
		h.t.Fatalf("boundary not reached; calls=%v child journal=%v", h.f.calls, events)
	}
}

func childStageSuccess() DispatchStageResult {
	return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}
}

// assertSingleFamily checks the parent continued exactly once from exactly
// one child execution, and that both histories replay at every boundary.
func (h *childRecoveryHarness) assertSingleFamily(run client.WorkflowRun) {
	h.t.Helper()
	if err := run.Get(h.ctx, nil); err != nil {
		h.t.Fatal(err)
	}
	if len(h.f.bindings) != 1 || len(h.f.envelopes) != 2 || len(h.f.terminal) != 1 || h.f.parentSlots != 1 {
		h.t.Fatalf("family did not settle once: bindings=%d envelopes=%d terminal=%d slots=%d calls=%v", len(h.f.bindings), len(h.f.envelopes), len(h.f.terminal), h.f.parentSlots, h.f.calls)
	}
	if got := len(parentEvents(h.t, h.dir, h.in.RunID, journal.ChildContinuedKind)); got != 1 {
		h.t.Fatalf("parent continued %d times", got)
	}
	parent := hitlFetchHistory(h.t, h.ctx, h.c, run.GetID(), run.GetRunID())
	starts, childID, childRunID := 0, "", ""
	for _, event := range parent.Events {
		if a := event.GetStartChildWorkflowExecutionInitiatedEventAttributes(); a != nil {
			starts++
			childID = a.WorkflowId
		}
		if a := event.GetChildWorkflowExecutionStartedEventAttributes(); a != nil {
			childRunID = a.WorkflowExecution.RunId
		}
	}
	if starts != 1 {
		h.t.Fatalf("parent issued %d child starts", starts)
	}
	replayEveryWorkflowTask(h.t, parent, replayChildEntry(run.GetID()))
	child := hitlFetchHistory(h.t, h.ctx, h.c, childID, childRunID)
	replayEveryWorkflowTask(h.t, child, func(t *testing.T, history *historypb.History) {
		t.Helper()
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
		if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: workflow.Execution{ID: childID, RunID: childRunID}}); err != nil {
			t.Fatal(err)
		}
	})
}

func activityIdentities(t *testing.T, ctx context.Context, c client.Client, workflowID string) map[string]bool {
	t.Helper()
	identities := map[string]bool{}
	for _, event := range hitlFetchHistory(t, ctx, c, workflowID, "").Events {
		if started := event.GetActivityTaskStartedEventAttributes(); started != nil {
			identities[started.GetIdentity()] = true
		}
	}
	return identities
}

func childExecutionID(h *childRecoveryHarness) string {
	for _, b := range h.f.bindings {
		return childWorkflowID(b)
	}
	return ""
}

func TestEngineChildRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)

	// A drained dispatch worker hands the child's live stage to its
	// replacement: no new attempt, the same dispatch identity, one family.
	t.Run("child stage survives dispatch worker drain", func(t *testing.T) {
		h := newChildRecoveryHarness(t, ctx, c, "drain")
		queue := childDispatchQueue + "-drain"
		root := h.startRoot("root")
		defer root.Stop()
		holding, held := make(chan DispatchStageInput, 1), make(chan struct{})
		old := h.startDispatch("before-drain", queue, func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
			holding <- input
			close(held)
			for {
				activity.RecordHeartbeat(ctx)
				select {
				case <-activity.GetWorkerStopChannel(ctx):
					return DispatchStageResult{}, dispatchWorkerHandoff(recoveryCustody())
				case <-ctx.Done():
					return DispatchStageResult{}, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
		run := h.start()
		h.await(held)
		first := <-holding
		old.Stop()
		var adopted atomic.Int32
		replacement := h.startDispatch("after-drain", queue, func(_ context.Context, input DispatchStageInput) (DispatchStageResult, error) {
			adopted.Add(1)
			if input.Custody == nil || *input.Custody != recoveryCustody() || input.Envelope.Attempt != first.Envelope.Attempt || input.Envelope.TaskID != first.Envelope.TaskID {
				t.Error("replacement changed the child's dispatch identity or lost custody")
			}
			return childStageSuccess(), nil
		})
		defer replacement.Stop()
		h.assertSingleFamily(run)
		if adopted.Load() != 1 {
			t.Fatalf("replacement adopted %d times", adopted.Load())
		}
		if ids := activityIdentities(t, ctx, c, childExecutionID(h)); !ids["before-drain"] || !ids["after-drain"] {
			t.Fatalf("child history did not record both dispatch workers: %v", ids)
		}
	})

	// The workflow worker hosting parent and child is replaced while the
	// child's stage runs. The new worker rebuilds both from history and
	// settles the family without re-admitting or restarting the child.
	t.Run("family survives workflow worker replacement", func(t *testing.T) {
		h := newChildRecoveryHarness(t, ctx, c, "rootloss")
		queue := childDispatchQueue + "-rootloss"
		running, release := make(chan struct{}), make(chan struct{})
		var once atomic.Bool
		dispatch := h.startDispatch("dispatch", queue, func(ctx context.Context, _ DispatchStageInput) (DispatchStageResult, error) {
			if once.CompareAndSwap(false, true) {
				close(running)
			}
			for {
				activity.RecordHeartbeat(ctx)
				select {
				case <-release:
					return childStageSuccess(), nil
				case <-ctx.Done():
					return DispatchStageResult{}, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
		defer dispatch.Stop()
		old := h.startRoot("root-before")
		run := h.start()
		h.await(running)
		old.Stop()
		replacement := h.startRoot("root-after")
		defer replacement.Stop()
		close(release)
		h.assertSingleFamily(run)
		admits := 0
		for _, call := range h.f.calls {
			if call == "admit" {
				admits++
			}
		}
		if admits != 1 {
			t.Fatalf("worker replacement re-admitted the child: %v", h.f.calls)
		}
	})

	// A dispatch worker that dies without handing off stops heartbeating.
	// The child's stage is retried on the infrastructure budget by the
	// replacement; the parent still continues exactly once.
	t.Run("child stage heartbeat loss retries on infrastructure budget", func(t *testing.T) {
		h := newChildRecoveryHarness(t, ctx, c, "heartbeat")
		queue := childDispatchQueue + "-heartbeat"
		// Detect the partitioned worker in seconds rather than the minute
		// production waits; the loss path is otherwise identical.
		previous := dispatchHeartbeatTimeoutV2
		dispatchHeartbeatTimeoutV2 = 3 * time.Second
		t.Cleanup(func() { dispatchHeartbeatTimeoutV2 = previous })
		root := h.startRoot("root")
		defer root.Stop()
		lost := make(chan struct{})
		stuck := make(chan struct{})
		defer close(stuck)
		dead := h.startDispatch("lost-worker", queue, func(context.Context, DispatchStageInput) (DispatchStageResult, error) {
			close(lost)
			<-stuck // never heartbeats, never returns: a partitioned worker
			return DispatchStageResult{}, fmt.Errorf("unreachable")
		})
		run := h.start()
		h.await(lost)
		go dead.Stop()
		var retried atomic.Int32
		replacement := h.startDispatch("after-loss", queue, func(_ context.Context, input DispatchStageInput) (DispatchStageResult, error) {
			retried.Add(1)
			if input.Class != journal.AttemptInfra {
				t.Errorf("heartbeat loss retried as %q, want infra", input.Class)
			}
			return childStageSuccess(), nil
		})
		defer replacement.Stop()
		h.assertSingleFamily(run)
		if retried.Load() != 1 {
			t.Fatalf("replacement ran %d attempts", retried.Load())
		}
		var starts []string
		for _, b := range h.f.bindings {
			for _, e := range liveEvents(t, h.dir, b.Child.RunID) {
				if e.Type == journal.EventStageStarted && e.Stage == "work" {
					starts = append(starts, fmt.Sprintf("%d/%s", e.Attempt, e.AttemptClass))
				}
			}
		}
		if fmt.Sprint(starts) != "[1/ 2/infra]" {
			t.Fatalf("child journal starts = %v, want attempt 1 then attempt 2 class infra", starts)
		}
	})

	// Phase 1 never restarts a sealed child. Resetting the parent to before
	// the child start re-issues the same stable child ID; the server rejects
	// the duplicate and the parent stops with the explicit refusal while the
	// sealed result stays retained.
	t.Run("sealed child restart is refused", func(t *testing.T) {
		h := newChildRecoveryHarness(t, ctx, c, "sealed")
		queue := childDispatchQueue + "-sealed"
		root := h.startRoot("root")
		defer root.Stop()
		dispatch := h.startDispatch("dispatch", queue, func(context.Context, DispatchStageInput) (DispatchStageResult, error) {
			return childStageSuccess(), nil
		})
		defer dispatch.Stop()
		run := h.start()
		if err := run.Get(ctx, nil); err != nil {
			t.Fatal(err)
		}
		history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
		var resetTo int64
		for _, event := range history.Events {
			if event.GetStartChildWorkflowExecutionInitiatedEventAttributes() != nil {
				break
			}
			if event.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED {
				resetTo = event.GetEventId()
			}
		}
		reset, err := c.ResetWorkflowExecution(ctx, &workflowservice.ResetWorkflowExecutionRequest{Namespace: "default", RequestId: "sealed-child-reset", Reason: "qualify sealed child restart refusal",
			WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: run.GetID(), RunId: run.GetRunID()}, WorkflowTaskFinishEventId: resetTo})
		if err != nil {
			t.Fatal(err)
		}
		err = c.GetWorkflow(ctx, run.GetID(), reset.GetRunId()).Get(ctx, nil)
		if err == nil || !strings.Contains(err.Error(), errSealedChildRestart.Error()) {
			t.Fatalf("reset parent did not refuse the sealed child: %v", err)
		}
		var result RunResult
		if err := c.GetWorkflow(ctx, childExecutionID(h), "").Get(ctx, &result); err != nil || result.ChildResult == nil || result.ChildResult.Input.State != "completed" {
			t.Fatalf("sealed child result was not retained: %+v %v", result, err)
		}
	})
}
