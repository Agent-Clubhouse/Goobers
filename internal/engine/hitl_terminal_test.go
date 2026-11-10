package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
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

// TestLiveTerminalDeterministicRetryDispatchesHumanAttempt pins that a
// retry-stage intent, which carries no addendum, still dispatches the retried
// task as the human attempt its stage.rerun.requested recorded.
func TestLiveTerminalDeterministicRetryDispatchesHumanAttempt(t *testing.T) {
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	spec := fixtureSpec("build", []apiv1.Task{{
		Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
		Run:  &apiv1.DeterministicRun{Command: []string{"build"}, Workspace: apiv1.WorkspaceScratch},
		Next: wf.TerminalComplete,
	}}, nil)
	exec := newScriptedExec(map[string][]scriptedCall{
		"build": {
			{result: apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Summary: "transient failure"}},
			{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "built"}},
		},
	})
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Journal: &terminalTestJournal{writer: writer}, Det: exec, Goober: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)})
	input := hitlInputFor(t, spec)
	input.RunID, input.LiveJournal = strings.Repeat("8", 32), true
	input.HITL.WaitSeconds = 10

	intent := baseIntent(HITLRetryStage, "terminal-retry")
	intent.RunID, intent.Stage, intent.ExpectedTerminalGeneration = input.RunID, "build", 1
	var ack HITLAck
	atTerminalHold(t, env, func() {
		env.UpdateWorkflow(HITLUpdateName, HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if err != nil {
				t.Error(err)
				return
			}
			ack = value.(HITLAck)
		}}, intent)
	})
	env.ExecuteWorkflow(Run, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !ack.Resumed || ack.Attempt != 2 || exec.calls["build"] != 2 {
		t.Fatalf("ack=%+v build calls=%d, want resumed attempt 2 and two dispatches", ack, exec.calls["build"])
	}
	var starts []journal.Event
	requested := false
	for _, event := range liveEvents(t, root, input.RunID) {
		switch {
		case event.Type == journal.EventStageRerunRequested && event.Stage == "build":
			requested = event.Attempt == 2 && event.AttemptClass == journal.AttemptHuman && event.InstructionAddendum == ""
		case event.Type == journal.EventStageStarted && event.Stage == "build":
			starts = append(starts, event)
		}
	}
	if !requested || len(starts) != 2 {
		t.Fatalf("requested=%v build starts=%d, want a human attempt-2 request and two starts", requested, len(starts))
	}
	if retried := starts[1]; retried.Attempt != 2 || retried.AttemptClass != journal.AttemptHuman {
		t.Fatalf("retried build stage.started attempt=%d class=%q, want attempt=2 class=%q",
			retried.Attempt, retried.AttemptClass, journal.AttemptHuman)
	}
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
	atTerminalHold(t, env, func() {
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
	})
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
	for _, refusal := range []error{journal.ErrInterventionConflict, journal.ErrInterventionStale, journal.ErrInterventionInvalid, livejournal.ErrTerminalAuthority} {
		t.Run(refusal.Error(), func(t *testing.T) {
			env, seam, root, input := terminalTestEnvironment(t)
			seam.refusal = refusal
			intent := terminalApproveIntent(input)
			refused := false
			atTerminalHold(t, env, func() {
				env.UpdateWorkflow(HITLUpdateName, HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) { refused = err != nil }}, intent)
			})
			env.ExecuteWorkflow(Run, input)
			if !refused || seam.calls != 1 {
				t.Fatalf("refused=%v calls=%d", refused, seam.calls)
			}
			for _, event := range liveEvents(t, root, input.RunID) {
				if event.Type == journal.EventRunResumed {
					t.Fatal("refused intent reopened")
				}
			}
		})
	}

}

func TestLiveTerminalCancellationFencesLostReply(t *testing.T) {
	env, seam, root, input := terminalTestEnvironment(t)
	seam.lost = 100
	intent := terminalApproveIntent(input)
	completed := false
	atTerminalHold(t, env, func() {
		env.UpdateWorkflow(HITLUpdateName, HITLIntentUpdateID(intent), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			if err == nil {
				t.Error("cancelled Update acknowledged")
			}
			completed = true
		}}, intent)
		env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 2*time.Second)
	})
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

// The activity host can be busy under race/parallel CI. Submit from the
// workflow's durable hold state, never a guessed offset from startup.
func atTerminalHold(t *testing.T, env *testsuite.TestWorkflowEnvironment, call func()) {
	t.Helper()
	var poll func()
	poll = func() {
		value, err := env.QueryWorkflow(HITLStateQuery)
		var state HITLState
		if err == nil {
			err = value.Get(&state)
		}
		if err == nil && state.Phase == hitlPhaseAwaiting {
			call()
			return
		}
		env.RegisterDelayedCallback(poll, 100*time.Millisecond)
	}
	env.RegisterDelayedCallback(poll, 100*time.Millisecond)
}

func TestHumanGateRerunResetsPolicyAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version workflow.Version
		want    int
	}{{"current", 1, 1}, {"legacy", workflow.DefaultVersion, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.OnGetVersion(humanGateRerunBudgetChange, workflow.DefaultVersion, 1).Return(tc.version)
			env.ExecuteWorkflow(func(ctx workflow.Context) (int, error) {
				rec := &runJournal{humanReruns: map[string]int{"review": 2}}
				attempts := map[string]int{"review": 1, "other": 3}
				evidence, prior := gateEvidence{}, "prior"
				rec.prepareHumanGateRerun(ctx, "review", &evidence, &prior, attempts)
				if attempts["other"] != 3 {
					return 0, errors.New("unrelated gate budget changed")
				}
				return attempts["review"] + 1, nil
			})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var attempt int
			if err := env.GetWorkflowResult(&attempt); err != nil {
				t.Fatal(err)
			}
			if attempt != tc.want {
				t.Fatalf("gate.started repassAttempt=%d want=%d", attempt, tc.want)
			}
		})
	}
}
