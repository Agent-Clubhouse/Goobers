package engine

import (
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestActivityJournalPinsTaskAndGateOwners(t *testing.T) {
	var rec runJournal
	at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	rec.stageStarted(at, apiv1.Task{Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer"}, 2, "")
	if op := rec.proj.Ops[0]; op.Time != at || op.Event.Runner["goober"] != "implementer" || op.Event.Attempt != 2 {
		t.Fatalf("task identity: %+v", op)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	flow := func(ctx workflow.Context) error {
		rec.gateStarted(ctx, apiv1.Gate{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}}, 3, 4)
		return nil
	}
	env.ExecuteWorkflow(flow)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	fields := rec.proj.Ops[1].Event.Runner
	if fields["goober"] != "reviewer" || fields["repassAttempt"] != 3 || fields["podAttempt"] != 4 {
		t.Fatalf("gate identity/counters: %v", fields)
	}
}
