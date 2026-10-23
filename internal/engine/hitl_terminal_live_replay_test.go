package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// lostTerminalReplies commits every delivery but suppresses every response
// until the test explicitly restores transport. The journal is never mocked.
type lostTerminalReplies struct {
	*livejournal.Writer
	lose  atomic.Bool
	calls atomic.Int32
}

func (j *lostTerminalReplies) CommitTerminalIntervention(ctx context.Context, req livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	j.calls.Add(1)
	receipt, err := j.Writer.CommitTerminalIntervention(ctx, req)
	if err == nil && j.lose.Load() {
		return journal.TerminalInterventionReceipt{}, errors.New("all terminal journal replies lost")
	}
	return receipt, err
}

func waitLiveHITL(t *testing.T, ctx context.Context, c client.Client, run client.WorkflowRun, accept func(HITLState) bool) HITLState {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), HITLStateQuery)
		if err == nil {
			var state HITLState
			if err := value.Get(&state); err != nil {
				t.Fatal(err)
			}
			if accept(state) {
				return state
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for HITL state: %v (last query %v)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func terminalReplayFixture(t *testing.T, c client.Client, index int) (client.WorkflowRun, *lostTerminalReplies, string, RunInput) {
	t.Helper()
	root := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	seam := &lostTerminalReplies{Writer: writer}
	if index == 0 {
		seam.lose.Store(true)
	}
	exec := hitlFailingExec()
	exec.verdicts["review"] = []apiv1.Verdict{{Decision: apiv1.VerdictNeedsChanges, Summary: "needs work"}, {Decision: apiv1.VerdictPass, Summary: "fixed"}}
	queue := fmt.Sprintf("live-terminal-%d", index)
	worker := temporalworker.New(c, queue, temporalworker.Options{})
	RegisterWith(worker, &Activities{Journal: seam, Goober: exec, Det: exec, Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	input := hitlInput(t)
	input.RunID = fmt.Sprintf("%032x", index+801)
	input.LiveJournal = true
	input.HITL.WaitSeconds = 30
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: input.RunID, TaskQueue: queue}, Run, input)
	if err != nil {
		t.Fatal(err)
	}
	return run, seam, root, input
}

func waitTerminalRetryEvidence(t *testing.T, ctx context.Context, c client.Client, run client.WorkflowRun) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
		if err != nil {
			t.Fatal(err)
		}
		for _, activity := range response.PendingActivities {
			if activity.GetActivityType().GetName() == ActCommitTerminalIntervention && activity.GetAttempt() >= 2 && strings.Contains(activity.GetLastFailure().GetMessage(), "all terminal journal replies lost") {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestLiveTerminalHistoriesReplayEveryWorkflowTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	for index, action := range []string{"approve", "override", "rerun", "deny"} {
		t.Run(action, func(t *testing.T) {
			run, seam, root, input := terminalReplayFixture(t, c, index)
			state := waitLiveHITL(t, ctx, c, run, func(s HITLState) bool { return s.Phase == hitlPhaseAwaiting })
			if !state.LiveTerminalInterventions {
				t.Fatal("live terminal capability absent")
			}
			intent := terminalApproveIntent(input)
			intent.Resolution = action
			intent.Rationale = "operator reviewed"
			intent.ExpectedTerminalGeneration = state.TerminalGeneration
			if action == "rerun" {
				intent.Kind, intent.Stage, intent.InstructionAddendum = HITLRerunStage, "implement", "fix review"
				intent.Gate, intent.Resolution, intent.Decision = "", "", ""
			}
			options := client.UpdateWorkflowOptions{WorkflowID: run.GetID(), RunID: run.GetRunID(), UpdateID: HITLIntentUpdateID(intent), UpdateName: HITLUpdateName, Args: []interface{}{intent}, WaitForStage: client.WorkflowUpdateStageAccepted}
			handle, err := c.UpdateWorkflow(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			if index == 0 {
				pending := waitLiveHITL(t, ctx, c, run, func(s HITLState) bool { return s.Delivery != nil })
				if pending.Delivery.Status != "intervention pending (journal delivery retrying)" || pending.Delivery.RequestID != intent.RequestID {
					t.Fatalf("pending=%+v", pending.Delivery)
				}
				waitTerminalRetryEvidence(t, ctx, c, run)
				seam.lose.Store(false)
			}
			var ack HITLAck
			if err := handle.Get(ctx, &ack); err != nil {
				t.Fatal(err)
			}
			retry, err := c.UpdateWorkflow(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			var duplicate HITLAck
			if err := retry.Get(ctx, &duplicate); err != nil || duplicate != ack {
				t.Fatalf("same UpdateID retry=%+v first=%+v err=%v", duplicate, ack, err)
			}
			if ack.Resumed != (action != "deny") {
				t.Fatalf("ack=%+v", ack)
			}
			var result RunResult
			if err := run.Get(ctx, &result); err != nil {
				t.Fatal(err)
			}
			events := liveEvents(t, root, input.RunID)
			transitions := 0
			for _, event := range events {
				if event.Type == journal.EventRunResumed || event.Type == journal.EventStageRerunRequested {
					transitions++
				}
			}
			want := 1
			if action == "deny" {
				want = 0
			}
			if transitions != want {
				t.Fatalf("transitions=%d want=%d", transitions, want)
			}
			history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
			commits := 0
			for _, event := range history.Events {
				if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil && scheduled.ActivityType.Name == ActCommitTerminalIntervention {
					commits++
				}
			}
			if commits != 1 {
				t.Fatalf("activity retry grew workflow history: %d terminal commands", commits)
			}
			if count := replayEveryWorkflowTask(t, history, hitlReplay); count < 3 {
				t.Fatalf("only %d workflow tasks", count)
			}
		})
	}
}
