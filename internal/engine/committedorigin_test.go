package engine

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func receiptFixture() (*runJournal, livejournal.Op, livejournal.ChildOriginReceipt) {
	const runID = "run-origin-receipt"
	rec := &runJournal{proj: JournalProjection{Identity: journal.RunIdentity{RunID: runID}}}
	op := livejournal.Op{Key: "start-key", Kind: livejournal.OpAppend, ChildStageStart: &livejournal.ChildStageStart{}, Event: &journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}}
	id := journal.StageAttemptID(runID, 0, "build", 23)
	receipt := livejournal.ChildOriginReceipt{Key: op.Key, Seq: 23, Stage: "build", Attempt: 1, Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: id, AttemptID: id}}
	return rec, op, receipt
}

func TestPreserveChildOriginsRejectsIncompleteOrMismatchedAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*livejournal.ChildOriginReceipt)
		missing bool
	}{
		{name: "missing", missing: true},
		{name: "wrong key", mutate: func(r *livejournal.ChildOriginReceipt) { r.Key = "other" }},
		{name: "projection ordinal", mutate: func(r *livejournal.ChildOriginReceipt) { r.Seq = 2 }},
		{name: "other stage", mutate: func(r *livejournal.ChildOriginReceipt) { r.Stage = "other" }},
		{name: "other attempt", mutate: func(r *livejournal.ChildOriginReceipt) { r.Attempt++ }},
		{name: "other class", mutate: func(r *livejournal.ChildOriginReceipt) { r.Class = journal.AttemptInfra }},
		{name: "other branch", mutate: func(r *livejournal.ChildOriginReceipt) { r.Branch++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, op, receipt := receiptFixture()
			if tc.mutate != nil {
				tc.mutate(&receipt)
			}
			receipts := []livejournal.ChildOriginReceipt{receipt}
			if tc.missing {
				receipts = nil
			}
			if rec.preserveChildOrigins([]livejournal.Op{op}, receipts) == nil || len(rec.childOrigins) != 0 {
				t.Fatal("invalid receipt preserved")
			}
		})
	}
	rec, op, receipt := receiptFixture()
	if err := rec.preserveChildOrigins([]livejournal.Op{op}, []livejournal.ChildOriginReceipt{receipt}); err != nil {
		t.Fatal(err)
	}
	if err := rec.preserveChildOrigins([]livejournal.Op{op}, []livejournal.ChildOriginReceipt{receipt}); err != nil {
		t.Fatal(err)
	}
	changed := receipt
	changed.Seq = 24
	id := journal.StageAttemptID(rec.proj.Identity.RunID, 0, "build", 24)
	changed.Origin = apiv1.ChildWorkflowOrigin{StageOccurrence: id, AttemptID: id}
	if rec.preserveChildOrigins([]livejournal.Op{op}, []livejournal.ChildOriginReceipt{changed}) == nil || rec.childOrigins[op.Key] != receipt {
		t.Fatal("retry replaced original committed receipt")
	}
	if rec.preserveChildOrigins([]livejournal.Op{op}, []livejournal.ChildOriginReceipt{receipt, receipt}) == nil {
		t.Fatal("duplicate receipt accepted")
	}
}

// This test-only workflow exercises the exact production emission method. The
// boolean is pinned in history; the production engine never opts in yet.
func originEmissionProbe(ctx workflow.Context, runID string, bind bool) (map[string]livejournal.ChildOriginReceipt, error) {
	spec := fixtureSpec("build", []apiv1.Task{detTask("build", wf.TerminalComplete)}, nil)
	in := runInput("origin-probe", spec)
	in.RunID = runID
	in.LiveJournal = true
	machine, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, Spec: in.Spec})
	if err != nil {
		return nil, err
	}
	rec, err := newRunJournalRecorder(in, machine)
	if err != nil {
		return nil, err
	}
	rec.runStarted(ctx)
	rec.stageStarted(workflow.Now(ctx), spec.Tasks[0], 1, "")
	if bind {
		rec.proj.Ops[len(rec.proj.Ops)-1].ChildStageStart = &livejournal.ChildStageStart{}
	}
	if err := rec.emitPending(ctx); err != nil {
		return nil, err
	}
	return rec.childOrigins, nil
}

func TestEmitPendingPreservesOnlyCommittedChildOrigins(t *testing.T) {
	for _, bind := range []bool{false, true} {
		writer, runsDir := newLiveWriter(t)
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		env.RegisterActivity(&Activities{Journal: &ackLossEmitter{writer: writer}})
		env.ExecuteWorkflow(originEmissionProbe, "run-probe", bind)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		var receipts map[string]livejournal.ChildOriginReceipt
		if err := env.GetWorkflowResult(&receipts); err != nil {
			t.Fatal(err)
		}
		if (len(receipts) == 1) != bind {
			t.Fatalf("bind=%v receipts=%v", bind, receipts)
		}
		reader, err := journal.OpenRead(filepath.Join(runsDir, "run-probe"))
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		starts := 0
		for _, event := range events {
			if event.Type == journal.EventStageStarted {
				starts++
			}
		}
		if starts != 1 {
			t.Fatalf("ack-loss retry appended %d stage starts", starts)
		}
	}
}

func TestChildOriginEmissionHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	writer, _ := newLiveWriter(t)
	worker := temporalworker.New(c, "origin-replay", temporalworker.Options{})
	worker.RegisterWorkflow(originEmissionProbe)
	worker.RegisterActivity(&Activities{Journal: writer})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	for _, tc := range []struct {
		id   string
		bind bool
	}{{"origin-legacy", false}, {"origin-bound", true}} {
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: tc.id, TaskQueue: "origin-replay"}, originEmissionProbe, tc.id, tc.bind)
		if err != nil {
			t.Fatal(err)
		}
		var receipts map[string]livejournal.ChildOriginReceipt
		if err := run.Get(ctx, &receipts); err != nil {
			t.Fatal(err)
		}
		history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(originEmissionProbe)
		if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatalf("bind=%v replay: %v", tc.bind, err)
		}
		// Independently recorded activity response carries exactly the returned
		// committed identity, so replay does not consult current journal state.
		var acknowledged []livejournal.ChildOriginReceipt
		for _, event := range history.Events {
			if a := event.GetActivityTaskCompletedEventAttributes(); a != nil {
				var response livejournal.EmitResponse
				if err := converter.GetDefaultDataConverter().FromPayloads(a.Result, &response); err != nil {
					t.Fatal(err)
				}
				acknowledged = append(acknowledged, response.ChildOrigins...)
			}
		}
		if len(acknowledged) != len(receipts) {
			t.Fatal("history lost committed receipts")
		}
		for _, receipt := range acknowledged {
			if !reflect.DeepEqual(receipts[receipt.Key], receipt) {
				t.Fatal("returned origin differs from history")
			}
		}
	}
}

func TestEmitPendingKeepsWatermarkWithoutChildReceipt(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActEmitJournal, mock.Anything, mock.Anything).Return(livejournal.EmitResponse{Seq: 99}, nil).Once()
	env.ExecuteWorkflow(func(ctx workflow.Context) (bool, error) {
		rec, op, _ := receiptFixture()
		rec.live = true
		rec.proj.Ops = []JournalOp{{Kind: opAppend, Event: op.Event, ChildStageStart: op.ChildStageStart, Time: workflow.Now(ctx)}}
		err := rec.emitPending(ctx)
		return err != nil && rec.emitted == 0 && len(rec.childOrigins) == 0, nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var refused bool
	if err := env.GetWorkflowResult(&refused); err != nil {
		t.Fatal(err)
	}
	if !refused {
		t.Fatal("missing receipt advanced emission watermark or invented authority")
	}
	env.AssertExpectations(t)
}
