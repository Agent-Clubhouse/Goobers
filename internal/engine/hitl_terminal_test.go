package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

type terminalTestJournal struct {
	writer       *livejournal.Writer
	lost         int
	calls        int
	refusal      error
	original     livejournal.TerminalInterventionRequest
	cancellation int
}

func (j *terminalTestJournal) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	return j.writer.Emit(ctx, req)
}
func (j *terminalTestJournal) CommitTerminalIntervention(ctx context.Context, req livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	if len(req.Transition.Cancellation) > 0 {
		j.cancellation++
		return j.writer.CommitTerminalIntervention(ctx, req)
	}
	j.calls++
	j.original = req
	if j.refusal != nil {
		return journal.TerminalInterventionReceipt{}, j.refusal
	}
	receipt, err := j.writer.CommitTerminalIntervention(ctx, req)
	if err != nil {
		return receipt, err
	}
	if j.calls <= j.lost {
		return journal.TerminalInterventionReceipt{}, errors.New("reply lost after commit")
	}
	return receipt, nil
}

func terminalTestEnvironment(t *testing.T) (*testsuite.TestWorkflowEnvironment, *terminalTestJournal, string, RunInput) {
	t.Helper()
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	seam := &terminalTestJournal{writer: writer}
	exec := hitlFailingExec()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Journal: seam, Det: exec, Goober: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)})
	input := hitlInput(t)
	input.RunID, input.LiveJournal = strings.Repeat("9", 32), true
	input.HITL.WaitSeconds = 10
	return env, seam, root, input
}

func terminalApproveIntent(input RunInput) HITLIntent {
	intent := baseIntent(HITLResolveEscalation, "terminal-approve")
	intent.RunID, intent.Gate, intent.Resolution, intent.Decision = input.RunID, "review", HITLResolutionApprove, "pass"
	intent.ExpectedTerminalGeneration = 1
	return intent
}

func TestLiveTerminalLostRepliesRetryOneIntentBeforeAcknowledgement(t *testing.T) {
	env, seam, root, input := terminalTestEnvironment(t)
	seam.lost = 4 // Backoff exceeds the original ten-second hold window.
	intent := terminalApproveIntent(input)
	var ack HITLAck
	completed := 0
	env.RegisterDelayedCallback(func() {
		state, err := env.QueryWorkflow(HITLStateQuery)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot HITLState
		if err := state.Get(&snapshot); err != nil {
			t.Fatal(err)
		}
		if !snapshot.LiveTerminalInterventions {
			t.Fatal("terminal capability absent")
		}
		env.UpdateWorkflow(HITLUpdateName, HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if err != nil {
				t.Error(err)
				return
			}
			ack = value.(HITLAck)
			completed++
			if seam.calls != 5 {
				t.Errorf("acked before all replies recovered: calls=%d", seam.calls)
			}
		}}, intent)
	}, time.Second)
	env.ExecuteWorkflow(Run, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if completed != 1 || !ack.Resumed || seam.calls != 5 {
		t.Fatalf("completed=%d ack=%+v calls=%d", completed, ack, seam.calls)
	}
	count := 0
	for _, event := range liveEvents(t, root, input.RunID) {
		if event.Type == journal.EventRunResumed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reopens=%d", count)
	}
}

func TestLiveTerminalDefinitiveRefusalDoesNotRetry(t *testing.T) {
	env, seam, root, input := terminalTestEnvironment(t)
	seam.refusal = journal.ErrInterventionConflict
	intent := terminalApproveIntent(input)
	refused := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(HITLUpdateName, HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) { refused = err != nil }}, intent)
	}, time.Second)
	env.ExecuteWorkflow(Run, input)
	if !refused || seam.calls != 1 {
		t.Fatalf("refused=%v calls=%d", refused, seam.calls)
	}
	for _, event := range liveEvents(t, root, input.RunID) {
		if event.Type == journal.EventRunResumed {
			t.Fatal("refused intent reopened")
		}
	}
}

func TestLiveTerminalCancellationFencesLostReply(t *testing.T) {
	env, seam, root, input := terminalTestEnvironment(t)
	seam.lost = 100
	intent := terminalApproveIntent(input)
	completed := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(HITLUpdateName, HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			if err == nil {
				t.Error("cancelled Update acknowledged")
			}
			completed = true
		}}, intent)
	}, time.Second)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 3*time.Second)
	env.ExecuteWorkflow(Run, input)
	if env.GetWorkflowError() == nil || seam.cancellation != 1 || !completed {
		t.Fatalf("err=%v cancellations=%d completed=%v", env.GetWorkflowError(), seam.cancellation, completed)
	}
	if _, err := seam.writer.CommitTerminalIntervention(t.Context(), seam.original); !errors.Is(err, journal.ErrInterventionCancelled) {
		t.Fatalf("late retry=%v", err)
	}
	events := liveEvents(t, root, input.RunID)
	if last := events[len(events)-1]; last.Type != journal.EventRunFinished || last.Status != "aborted" {
		t.Fatalf("last=%+v", last)
	}
}
