package engine

// Replay coverage for #3883's HITL protocol.
//
// The test environment does not replay: it executes the workflow function once
// and hands back the result. Only worker.NewWorkflowReplayer over a history
// recorded by a real server re-runs the workflow function against Temporal's
// own determinism check. That check is the whole risk surface for this change,
// because the protocol adds three things a replay can trip over:
//
//   - AN UPDATE HANDLER. Update acceptance and completion are history events.
//     A worker that registered the handler under a different name, or that
//     decided differently about the same intent, wedges the run.
//   - A NEW TIMER AND AWAIT. settle parks the walk in AwaitWithTimeout, which
//     is a real timer command. A history that took the hold must reproduce it,
//     and a history that never had one must NOT grow one.
//   - AN EXTRA JOURNAL WRITE. The terminal is journaled before the hold and
//     the resumption after, so the emit-key sequence differs from a run that
//     settled straight through.
//
// The second bullet is the invariance requirement #3883 turns on: every run
// recorded before this commit has a nil HITLPolicy, and a worker carrying this
// code must replay those byte-for-byte. TestHITLPreProtocolHistoryReplays
// pins it by recording a history with the policy unset and replaying it
// against the new workflow function.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// hitlReplayServer starts one dev server for the file's tests.
func hitlReplayServer(t *testing.T, ctx context.Context) client.Client {
	t.Helper()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	return server.Client()
}

// hitlReplayWorker registers the engine on a task queue with a scripted exec.
func hitlReplayWorker(t *testing.T, c client.Client, taskQueue string, exec *scriptedExec) {
	t.Helper()
	w := temporalworker.New(c, taskQueue, temporalworker.Options{})
	RegisterWith(w, &Activities{
		Goober:     exec,
		Det:        exec,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
	})
	if err := w.Start(); err != nil {
		t.Fatalf("start Temporal worker: %v", err)
	}
	t.Cleanup(w.Stop)
}

// hitlFetchHistory pulls a completed run's whole history.
func hitlFetchHistory(t *testing.T, ctx context.Context, c client.Client, workflowID, runID string) *historypb.History {
	t.Helper()
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read workflow history: %v", err)
		}
		history.Events = append(history.Events, event)
	}
	return history
}

// hitlReplay runs the recorded history back through the real replayer.
func hitlReplay(t *testing.T, history *historypb.History) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay workflow history: %v", err)
	}
}

// hitlRunsAcceptedUpdatesReplay is the headline replay proof: a history that
// CONTAINS an accepted operator update — the hold timer, the update
// acceptance, the resumption, and the stages that followed it — replays
// cleanly through the same workflow function.
//
// It also proves the durability half of "never acknowledge success before
// durable workflow acceptance": the update is submitted with
// WaitForStage: WorkflowUpdateStageCompleted and its result read with
// handle.Get, so by the time the assertion below runs, the acceptance and the
// completion are already in the history this test then replays.
func TestHITLAcceptedUpdateHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)

	const taskQueue = "hitl-replay"
	exec := hitlFailingExec()
	hitlReplayWorker(t, c, taskQueue, exec)

	in := hitlInput(t)
	in.RunID = "hitl-replay-accepted"
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        in.RunID,
		TaskQueue: taskQueue,
	}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}

	// Wait for the run to reach the operator hold. The state query is the
	// protocol's own readiness signal, which is exactly what the daemon polls.
	deadline := time.Now().Add(90 * time.Second)
	var state HITLState
	for {
		if time.Now().After(deadline) {
			t.Fatalf("run never reached the operator hold; last state %+v", state)
		}
		resp, qerr := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), HITLStateQuery)
		if qerr == nil {
			if derr := resp.Get(&state); derr != nil {
				t.Fatalf("decode hitl state: %v", derr)
			}
			if state.Phase == hitlPhaseAwaiting {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if state.TerminalStatus != StatusEscalated {
		t.Fatalf("held terminal status = %q, want %q", state.TerminalStatus, StatusEscalated)
	}

	intent := baseIntent(HITLResolveEscalation, "replay-req-1")
	intent.RunID = in.RunID
	intent.Gate = "review"
	intent.Resolution = HITLResolutionApprove
	intent.Decision = "pass"
	intent.ExpectedTerminalGeneration = state.TerminalGeneration

	handle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   run.GetID(),
		RunID:        run.GetRunID(),
		UpdateID:     HITLIntentUpdateID(intent),
		UpdateName:   HITLUpdateName,
		Args:         []interface{}{intent},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatalf("submit hitl update: %v", err)
	}
	var ack HITLAck
	if err := handle.Get(ctx, &ack); err != nil {
		t.Fatalf("hitl update refused: %v", err)
	}
	if !ack.Resumed || ack.ResumeState != "ship" {
		t.Fatalf("ack = %+v, want a resume at ship", ack)
	}

	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q — the resolved run must finish through the branch target",
			result.Status, StatusCompleted)
	}

	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	if !hitlHistoryHasUpdate(history) {
		t.Fatal("recorded history contains no update events; this replay would prove nothing")
	}
	hitlReplay(t, history)
}

// TestHITLPreProtocolHistoryReplays is the invariance proof. A run whose input
// carries no HITLPolicy — which is every run recorded before #3883, because
// RunInput.HITL is omitempty and absent from those payloads — must produce the
// same command sequence under the new workflow function as it did under the
// old one. If settle ever grew an unconditional timer or journal write, this
// is the test that fails.
func TestHITLPreProtocolHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)

	const taskQueue = "hitl-replay-legacy"
	exec := hitlFailingExec()
	hitlReplayWorker(t, c, taskQueue, exec)

	in := hitlInput(t)
	in.RunID = "hitl-replay-legacy"
	in.HITL = nil

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        in.RunID,
		TaskQueue: taskQueue,
	}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q — a run with no policy must settle escalated, not hold",
			result.Status, StatusEscalated)
	}

	// An intent addressed to it is refused by name rather than buffered, and
	// the refusal leaves the settled run exactly as settled.
	intent := baseIntent(HITLResumeFromTerminal, "legacy-req-1")
	intent.RunID = in.RunID
	intent.Complete = true
	handle, uerr := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   run.GetID(),
		UpdateID:     HITLIntentUpdateID(intent),
		UpdateName:   HITLUpdateName,
		Args:         []interface{}{intent},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if uerr == nil {
		var ack HITLAck
		uerr = handle.Get(ctx, &ack)
	}
	if uerr == nil {
		t.Fatal("an intent addressed to a settled, policy-free run was accepted")
	}

	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	hitlReplay(t, history)
}

// TestHITLWindowExpiryHistoryReplays covers the third shape: the hold was
// taken but nobody answered, so the history contains the timer FIRING rather
// than an update. That is a different command sequence from both of the above
// and is the one an unattended production escalation actually records.
func TestHITLWindowExpiryHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)

	const taskQueue = "hitl-replay-expiry"
	exec := hitlFailingExec()
	hitlReplayWorker(t, c, taskQueue, exec)

	in := hitlInput(t)
	in.RunID = "hitl-replay-expiry"
	in.HITL.WaitSeconds = 1

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        in.RunID,
		TaskQueue: taskQueue,
	}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q — an unanswered hold leaves the terminal standing",
			result.Status, StatusEscalated)
	}
	if exec.calls["ship"] != 0 {
		t.Fatalf("ship dispatched %d times, want 0 — an expired hold must resume nothing", exec.calls["ship"])
	}

	// The terminal was journaled once, before the hold — not twice.
	events := hitlEvents(hitlReplayProjection(t, ctx, c, run.GetID(), run.GetRunID()))
	if got := hitlCountEvents(events, journal.EventRunFinished); got != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1 (types: %v)", got, hitlEventTypes(events))
	}

	hitlReplay(t, hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID()))
}

// hitlReplayProjection reads the journal projection off a finished real run.
func hitlReplayProjection(t *testing.T, ctx context.Context, c client.Client, workflowID, runID string) JournalProjection {
	t.Helper()
	resp, err := c.QueryWorkflow(ctx, workflowID, runID, JournalQuery)
	if err != nil {
		t.Fatalf("query journal projection: %v", err)
	}
	var proj JournalProjection
	if err := resp.Get(&proj); err != nil {
		t.Fatalf("decode journal projection: %v", err)
	}
	return proj
}

// hitlHistoryHasUpdate guards the headline replay against silently degrading
// into a replay of a history with no update in it.
func hitlHistoryHasUpdate(history *historypb.History) bool {
	for _, event := range history.Events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED,
			enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_COMPLETED:
			return true
		}
	}
	return false
}

// holdingGoober blocks the first implement invocation until released, so a
// real server delivery lands while the run is still executing.
type holdingGoober struct {
	*scriptedExec
	started, release chan struct{}
	once             sync.Once
}

func (g *holdingGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if env.Goal == "implement" {
		g.once.Do(func() {
			close(g.started)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		})
	}
	return g.scriptedExec.Invoke(ctx, env)
}

// TestHITLReissueWithSameBoundUpdateIDAfterRefusalSucceeds is the router's
// real retry on a real server: an intent refused mid-execution leaves no
// trace, so the identical intent under the same bound UpdateID succeeds once
// the run holds its terminal. Validator rejections are not deduplicated.
func TestHITLReissueWithSameBoundUpdateIDAfterRefusalSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	const taskQueue = "hitl-reissue"
	exec := hitlFailingExec()
	goober := &holdingGoober{scriptedExec: exec, started: make(chan struct{}), release: make(chan struct{})}
	w := temporalworker.New(c, taskQueue, temporalworker.Options{})
	RegisterWith(w, &Activities{Goober: goober, Det: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	in := hitlInput(t)
	in.RunID = "hitl-reissue"
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.RunID, TaskQueue: taskQueue}, Run, in)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-goober.started:
	case <-ctx.Done():
		t.Fatal("implement never started")
	}
	intent := baseIntent(HITLResolveEscalation, "reissue-req")
	intent.RunID, intent.Gate, intent.Resolution, intent.Decision = in.RunID, "review", HITLResolutionApprove, "pass"
	options := client.UpdateWorkflowOptions{WorkflowID: run.GetID(), RunID: run.GetRunID(), UpdateID: HITLIntentUpdateID(intent),
		UpdateName: HITLUpdateName, Args: []interface{}{intent}, WaitForStage: client.WorkflowUpdateStageCompleted}
	refused := func() error {
		handle, err := c.UpdateWorkflow(ctx, options)
		if err != nil {
			return err
		}
		var ack HITLAck
		return handle.Get(ctx, &ack)
	}()
	if refused == nil || !strings.Contains(refused.Error(), HITLErrRunExecuting) {
		t.Fatalf("mid-execution delivery = %v, want the %s refusal", refused, HITLErrRunExecuting)
	}
	close(goober.release)
	deadline := time.Now().Add(90 * time.Second)
	for {
		var state HITLState
		if resp, qerr := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), HITLStateQuery); qerr == nil && resp.Get(&state) == nil && state.Phase == hitlPhaseAwaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never reached the operator hold")
		}
		time.Sleep(200 * time.Millisecond)
	}
	handle, err := c.UpdateWorkflow(ctx, options)
	if err != nil {
		t.Fatalf("reissue with the same bound UpdateID: %v", err)
	}
	var ack HITLAck
	if err := handle.Get(ctx, &ack); err != nil || !ack.Resumed || ack.IntentDigest != HITLIntentDigest(intent) {
		t.Fatalf("reissued ack = %+v %v, want an accepted resume bound to the intent", ack, err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil || result.Status != StatusCompleted {
		t.Fatalf("resolved run = %+v %v", result, err)
	}
	hitlReplay(t, hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID()))
}
