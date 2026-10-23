package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// receiptDroppingEmitter commits every op but loses the per-op child-origin
// receipts, as an old writer or a lost acknowledgement would.
type receiptDroppingEmitter struct{ inner JournalEmitter }

func (e receiptDroppingEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	resp, err := e.inner.Emit(ctx, req)
	resp.ChildOrigins = nil
	return resp, err
}

func TestCommittedChildOriginRequired(t *testing.T) {
	t.Run("dispatch carries the writer receipt", func(t *testing.T) {
		f := newChildFixture()
		env, dir := childEnvironment(t, f)
		in := childParentInput()
		env.ExecuteWorkflow("Run", in)
		if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), errChildExecutionUnavailable.Error()) {
			t.Fatalf("admitted child crossed the HA-P4-08 activation boundary: %v", err)
		}
		var started journal.Event
		for _, event := range liveEvents(t, dir, in.RunID) {
			if event.Type == journal.EventStageStarted && event.Stage == "parent" {
				started = event
			}
		}
		origin, err := journal.ChildWorkflowOriginForEvent(in.RunID, started)
		if err != nil {
			t.Fatalf("stage start was not bound by the writer: %v", err)
		}
		if len(f.envelopes) != 1 || f.envelopes[0].ChildWorkflowOrigin == nil || *f.envelopes[0].ChildWorkflowOrigin != *origin || f.envelopes[0].Goober != "coder" {
			t.Fatalf("dispatch origin differs from the committed start: %+v want %+v", f.envelopes, origin)
		}
		if origin.StageOccurrence != journal.StageAttemptID(in.RunID, 0, "parent", started.Seq) || origin.AttemptID != origin.StageOccurrence {
			t.Fatalf("origin is not the committed sequence identity: %+v seq=%d", origin, started.Seq)
		}
		if len(f.admissions) != 1 || f.admissions[0].OriginSeq != started.Seq || f.admissions[0].Request.Origin != *origin {
			t.Fatalf("admission was not bound to the committed origin: %+v", f.admissions)
		}
		if !slices.Equal(f.calls, []string{"dispatch", "admit", "cancel"}) {
			t.Fatalf("admission was not fenced after the activation boundary: %v", f.calls)
		}
	})

	t.Run("lost receipts fail before dispatch", func(t *testing.T) {
		f := newChildFixture()
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		writer, _ := newLiveWriter(t)
		registerChildProtocol(env, &Activities{EngineChildren: f, Journal: receiptDroppingEmitter{writer}, Auto: gate.NewAutomatedEvaluator()}, f)
		env.ExecuteWorkflow("Run", childParentInput())
		if env.GetWorkflowError() == nil || len(f.envelopes) != 0 || len(f.admissions) != 0 {
			t.Fatalf("a watermark substituted for committed identity: err=%v calls=%v", env.GetWorkflowError(), f.calls)
		}
	})

	t.Run("forged origin is refused before admission", func(t *testing.T) {
		f := newChildFixture()
		f.forgeOrigin = true
		env, _ := childEnvironment(t, f)
		env.ExecuteWorkflow("Run", childParentInput())
		if env.GetWorkflowError() == nil || len(f.admissions) != 0 || len(f.envelopes) != 1 {
			t.Fatalf("forged origin reached admission: err=%v calls=%v", env.GetWorkflowError(), f.calls)
		}
	})

	t.Run("handoff outside the protocol fails closed", func(t *testing.T) {
		f := newChildFixture()
		env, _ := childEnvironment(t, f)
		in := runInput("ordinary", fixtureSpec("work", []apiv1.Task{{Name: "work", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "work", Workspace: apiv1.WorkspaceScratch, Next: wf.TerminalComplete}}, nil))
		in.LiveJournal = true
		env.ExecuteWorkflow("Run", in)
		err := env.GetWorkflowError()
		if err == nil || !strings.Contains(err.Error(), "child handoff is disabled") || len(f.admissions) != 0 || f.envelopes[0].ChildWorkflowOrigin != nil {
			t.Fatalf("ordinary stage acquired child authority: err=%v envelopes=%+v", err, f.envelopes)
		}
	})

	t.Run("unbound or other-attempt starts have no origin", func(t *testing.T) {
		task := childParentInput().Spec.Tasks[0]
		op := JournalOp{Kind: opAppend, EmitKey: "k", Event: &journal.Event{Type: journal.EventStageStarted, Stage: task.Name, Attempt: 1}}
		receipt := livejournal.ChildOriginReceipt{Key: "k", Seq: 7, Stage: task.Name, Attempt: 1}
		r := &runJournal{proj: JournalProjection{Ops: []JournalOp{op}}, childOrigins: map[string]livejournal.ChildOriginReceipt{"k": receipt}}
		if r.committedChildReceipt(task.Name, 1) != nil {
			t.Fatal("a receipt for an unbound start authorized a child")
		}
		r.proj.Ops[0].ChildStageStart = &livejournal.ChildStageStart{}
		if r.committedChildReceipt(task.Name, 2) != nil || r.committedChildReceipt(task.Name, 1) == nil {
			t.Fatal("receipt lookup is not bound to the exact attempt")
		}
		delete(r.childOrigins, "k")
		if r.committedChildReceipt(task.Name, 1) != nil {
			t.Fatal("projection ordinal substituted for an acknowledged receipt")
		}
	})
}

func TestEngineChildAdmissionReceiptMustMatchIntent(t *testing.T) {
	f := newChildFixture()
	in := ChildAdmissionInput{ParentWorkflow: "child-parent", ParentWorkflowID: "wf", ParentTemporalRunID: "trun", Stage: "parent", OriginSeq: 3}
	in.Request = journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: "run-child-parent", RequestID: journal.Digest([]byte("r")), Action: "wait", ChildRunID: "child-1",
		AcceptanceID: "trigger-child-1", InvocationKey: "child-1", Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: journal.StageAttemptID("run-child-parent", 0, "parent", 3), AttemptID: journal.StageAttemptID("run-child-parent", 0, "parent", 3)}}
	in.Request.SourceDigest = journal.Digest(childSourceBytes(in.Request))
	a := &Activities{EngineChildren: f}
	binding, err := a.AdmitEngineChild(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	changed := in
	changed.ParentTemporalRunID = "other"
	f.bindings[in.Request.RequestID] = binding
	if _, err := a.AdmitEngineChild(t.Context(), changed); err == nil {
		t.Fatal("admission accepted a receipt issued for another parent execution")
	}
	if _, err := (&Activities{}).AdmitEngineChild(t.Context(), in); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured admission = %v", err)
	}
}

func TestEngineChildSlotBoundsPerOccurrenceAndRun(t *testing.T) {
	task := childParentInput().Spec.Tasks[0]
	task.ChildWorkflows.MaxChildren = 2
	r := &runJournal{}
	plan := childAdmissionPlan{Task: task, Origin: livejournal.ChildOriginReceipt{Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: "a"}}}
	for i := range 2 {
		if err := r.reserveChildSlot(plan); err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
	}
	if err := r.reserveChildSlot(plan); err == nil {
		t.Fatal("occurrence bound not enforced")
	}
	r.children.total = apiv1.MaxChildWorkflows
	plan.Origin.Origin.StageOccurrence = "b"
	if err := r.reserveChildSlot(plan); err == nil {
		t.Fatal("run bound not enforced")
	}
}
