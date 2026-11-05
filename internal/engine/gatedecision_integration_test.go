package engine

import (
	"errors"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestGateDecisionBoundToOccurrence(t *testing.T) {
	writer, dir := newLiveWriter(t)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Journal: writer})
	first := engineGateDecisionRequest("first", 2, "needs-changes")
	stale := engineGateDecisionRequest("stale", 2, "pass")
	current := engineGateDecisionRequest("current", 4, "pass")
	var staleErr error
	emits := 0
	env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
		if err != nil || info.ActivityType.Name != ActEmitJournal {
			return
		}
		emits++
		switch emits {
		case 2:
			env.UpdateWorkflow(GateDecisionUpdateName, GateDecisionUpdateID(first), gateDecisionCallbacks(t, nil, nil), first)
		case 4:
			env.UpdateWorkflow(GateDecisionUpdateName, GateDecisionUpdateID(stale), gateDecisionCallbacks(t, nil, &staleErr), stale)
			env.UpdateWorkflow(GateDecisionUpdateName, GateDecisionUpdateID(current), gateDecisionCallbacks(t, nil, nil), current)
		}
	})

	in := runInput("human-occurrence", repeatedHumanGateSpec())
	in.RunID = "human-occurrence"
	in.TriggerKind = string(journal.TriggerManual)
	in.LiveJournal = true
	in.HITL = &HITLPolicy{Enabled: true, WaitSeconds: 60, Actors: []string{"operator"}}
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("result=%+v", result)
	}
	code, _, ok := gateDecisionRefusalCode(staleErr)
	if !ok || code != GateDecisionErrOccurrence {
		t.Fatalf("stale error=%v code=%q", staleErr, code)
	}
	events := liveEvents(t, dir, in.RunID)
	var pauses []uint64
	var actors []string
	for _, event := range events {
		switch event.Type {
		case journal.EventGatePaused:
			pauses = append(pauses, event.Seq)
		case journal.EventGateEvaluated:
			actors = append(actors, event.Actor)
		}
	}
	if len(pauses) != 2 || pauses[0] != 2 || pauses[1] != 4 ||
		len(actors) != 2 || actors[0] != "operator" || actors[1] != "operator" {
		t.Fatalf("pauses=%v actors=%v events=%+v", pauses, actors, events)
	}
	if err := diffConformanceViews(localHumanGateEvents(t), events); err != nil {
		t.Fatalf("runner/engine human gate parity: %v", err)
	}
}

func localHumanGateEvents(t *testing.T) []journal.Event {
	t.Helper()
	identity := journal.RunIdentity{
		RunID: "human-local", Workflow: "human", WorkflowVersion: 1,
		WorkflowDigest: journal.Digest([]byte("human-definition")), Gaggle: "web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}
	run, err := journal.Create(t.TempDir(), identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	human := repeatedHumanGateSpec().Gates[0]
	evaluator := &gate.Evaluator{Journal: run}
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: human.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluator.EvaluateHuman(human, "needs-changes", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: human.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluator.EvaluateHuman(human, "pass", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted), Disposition: journal.RunDispositionProduced}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestHumanGateWindowExpiredIsJournaledWithOccurrence(t *testing.T) {
	writer, dir := newLiveWriter(t)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Journal: writer})
	in := runInput("human-expiry", repeatedHumanGateSpec())
	in.RunID = "human-expiry"
	in.TriggerKind = string(journal.TriggerManual)
	in.LiveJournal = true
	in.HITL = &HITLPolicy{Enabled: true, WaitSeconds: 1}
	env.ExecuteWorkflow(Run, in)
	err := env.GetWorkflowError()
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != HumanGateWindowExpiredCode {
		t.Fatalf("workflow error=%v", err)
	}
	events := liveEvents(t, dir, in.RunID)
	found := false
	for _, event := range events {
		if event.Type == journal.EventError && event.Gate == "approval" &&
			event.Error != nil && event.Error.Code == HumanGateWindowExpiredCode &&
			event.Runner["gateOccurrence"] == float64(2) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expiry occurrence not journaled: %+v", events)
	}
}

func repeatedHumanGateSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web", Start: "approval",
		Gates: []apiv1.Gate{{
			Name: "approval", Evaluator: apiv1.EvaluatorHuman,
			Human: &apiv1.HumanGate{Approvers: []string{"operator"}},
			Branches: map[string]string{
				"pass": wf.TerminalComplete, "needs-changes": "approval",
			},
		}},
	}
}

func engineGateDecisionRequest(id string, occurrence uint64, decision string) GateDecisionRequest {
	return GateDecisionRequest{
		Protocol: GateDecisionProtocol, Version: GateDecisionProtocolVersion,
		RunID: "human-occurrence", RequestID: id, Actor: "operator",
		Gate: "approval", GateOccurrence: occurrence, Decision: decision,
	}
}

func gateDecisionCallbacks(t *testing.T, ack *GateDecisionAck, rejected *error) *testsuite.TestUpdateCallback {
	t.Helper()
	return &testsuite.TestUpdateCallback{
		OnReject: func(err error) {
			if rejected != nil {
				*rejected = err
			}
		},
		OnComplete: func(value interface{}, err error) {
			if err != nil || ack == nil || value == nil {
				return
			}
			switch result := value.(type) {
			case GateDecisionAck:
				*ack = result
			case converter.EncodedValue:
				if decodeErr := result.Get(ack); decodeErr != nil {
					t.Error(decodeErr)
				}
			}
		},
	}
}
