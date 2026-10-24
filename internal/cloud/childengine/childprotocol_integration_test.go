//go:build cloudintegration

package childengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type recordedCapacity struct{ released, reacquired int }

func (c *recordedCapacity) Release(context.Context, engine.ChildParkInput) error {
	c.released++
	return nil
}
func (c *recordedCapacity) Reacquire(context.Context, engine.ChildCompletionInput) error {
	c.reacquired++
	return nil
}

type recordedContinuations struct{ written [][]byte }

func (w *recordedContinuations) WriteChildContinuation(_ context.Context, _ engine.ChildRunBinding, _ livejournal.AppendReceipt, data []byte) (apiv1.ContextPointer, error) {
	w.written = append(w.written, data)
	return apiv1.ContextPointer{Name: "child-completion", Artifact: &apiv1.ArtifactPointer{Path: "stages/plan/child-completion.json", Digest: journal.Digest(data), Size: int64(len(data))}}, nil
}

// protocolFamily is a bound engine child with its own journal, served by the
// control-side child protocol.
type protocolFamily struct {
	*childWorkflowFamily
	binding engine.ChildRunBinding
	child   *journal.Run
	service *Service
}

func newProtocolFamily(t *testing.T) *protocolFamily {
	t.Helper()
	f := &protocolFamily{childWorkflowFamily: newChildWorkflowFamily(t)}
	run := f.parent.pins.RunID
	started, err := f.childWorkflowFamily.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, run, "call-1", []byte(generatedChild))
	if err != nil {
		t.Fatal(err)
	}
	f.child, err = journal.Create(t.TempDir(), journal.RunIdentity{RunID: started.RunID, Workflow: "generated-check", WorkflowVersion: 1, Gaggle: f.parent.pins.Gaggle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.child.Close() })
	f.service = &Service{Custody: f.controller, Freeze: freezeForTest, Wait: time.Hour, Actor: "control-plane", Now: time.Now,
		Journal: func(_ context.Context, runID string) (journal.RunReader, error) {
			if runID != started.RunID {
				return nil, errors.New("not the child's journal")
			}
			return journal.OpenReadOnly(f.child.Dir())
		}}
	request := journal.ChildHandoffRequest{Gaggle: f.parent.pins.Gaggle, ParentRunID: run, RequestID: journal.Digest([]byte(started.RunID)),
		Action: "wait", ChildRunID: started.RunID, AcceptanceID: started.AcceptanceID, InvocationKey: "call-1", SourceDigest: started.SourceDigest, Origin: f.parent.origin}
	f.binding, err = f.service.Admit(t.Context(), engine.ChildAdmissionInput{Request: request, ParentWorkflow: f.parent.pins.Workflow, ParentWorkflowID: run,
		ParentTemporalRunID: "temporal-run-1", Stage: "plan", OriginSeq: 2})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The finalizer seals the child's result from its own journal: the state and
// finish time its workflow decided, its latest stage summary and its readable
// references. A retry returns the same seal; a changed one is refused.
func TestFinalizeSealsTheChildResultFromItsJournal(t *testing.T) {
	f := newProtocolFamily(t)
	report, err := f.child.RecordArtifact("report.md", []byte("child report"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.child.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Outputs: map[string]any{"summary": "checked"}, Artifacts: []journal.Ref{report}}); err != nil {
		t.Fatal(err)
	}
	finished := f.binding.Child.AcceptedAt.Add(time.Second)
	in := engine.ChildFinalizeInput{Binding: f.binding, State: triggerqueue.ChildCompleted, FinishedAt: finished}
	sealed, err := f.service.Finalize(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealed.Validate(f.binding.Child); err != nil {
		t.Fatalf("sealed result does not validate as the engine checks it: %v", err)
	}
	if sealed.Input.State != triggerqueue.ChildCompleted || sealed.Input.Summary != "checked" || len(sealed.Input.References) != 1 || sealed.Input.References[0] != report.Digest {
		t.Fatalf("sealed = %+v", sealed.Input)
	}
	child, err := f.controller.GetChild(t.Context(), f.binding.Child.Identity)
	if err != nil || child.State != triggerqueue.ChildCompleted || child.ResultRef != sealed.ResultRef {
		t.Fatalf("custody = %+v, %v; want the child terminal with the sealed result", child, err)
	}
	if again, err := f.service.Finalize(t.Context(), in); err != nil || again.ResultRef != sealed.ResultRef {
		t.Fatalf("repeated finalize = %+v, %v", again, err)
	}
	changed := in
	changed.State = triggerqueue.ChildFailed
	if _, err := f.service.Finalize(t.Context(), changed); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("finalize with a changed state = %v, want a refusal", err)
	}
}

// A cancelled child's end still goes through the finalizer.
func TestFinalizeSealsACancelledChild(t *testing.T) {
	f := newProtocolFamily(t)
	if err := f.service.Cancel(t.Context(), f.binding); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.service.Finalize(t.Context(), engine.ChildFinalizeInput{Binding: f.binding, State: triggerqueue.ChildCancelled, FinishedAt: time.Now().UTC()})
	if err != nil || sealed.Input.State != triggerqueue.ChildCancelled || sealed.Input.Summary != "Child workflow cancelled" {
		t.Fatalf("cancelled finalize = %+v, %v", sealed, err)
	}
}

// Parking and resuming need the capacity plane and the continuation path;
// without them both are a named unavailability, never a silent no-op that
// would keep a parked parent's permit.
func TestParkAndCompleteNeedTheCapacityPlaneAndContinuationPath(t *testing.T) {
	f := newProtocolFamily(t)
	wait := livejournal.AppendReceipt{Key: "wait-1", Seq: 7, Type: journal.EventStageFinished}
	if err := f.service.Park(t.Context(), engine.ChildParkInput{Binding: f.binding, Wait: wait}); !errors.Is(err, engine.ErrChildCustodyUnavailable) || !errors.Is(err, ErrCapacityPlaneNotComposed) {
		t.Fatalf("park without a capacity plane = %v", err)
	}
	sealed, err := f.service.Finalize(t.Context(), engine.ChildFinalizeInput{Binding: f.binding, State: triggerqueue.ChildCompleted, FinishedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	completion := engine.ChildCompletionInput{Binding: f.binding, Wait: wait, Result: sealed}
	if _, err := f.service.Complete(t.Context(), completion); !errors.Is(err, ErrContinuationNotComposed) {
		t.Fatalf("complete without a continuation path = %v", err)
	}

	capacity, continuations := &recordedCapacity{}, &recordedContinuations{}
	f.service.Capacity, f.service.Continuations = capacity, continuations
	if err := f.service.Park(t.Context(), engine.ChildParkInput{Binding: f.binding, Wait: wait}); err != nil || capacity.released != 1 {
		t.Fatalf("park = %v (released %d)", err, capacity.released)
	}
	forged := completion
	forged.Result.Input.Summary = "a result the child never sealed"
	if _, err := f.service.Complete(t.Context(), forged); !errors.Is(err, engine.ErrChildCustodyRefused) || len(continuations.written) != 0 {
		t.Fatalf("complete with a forged result = %v (wrote %d)", err, len(continuations.written))
	}
	pointer, err := f.service.Complete(t.Context(), completion)
	if err != nil || capacity.reacquired != 1 || len(continuations.written) != 1 || pointer.Artifact == nil {
		t.Fatalf("complete = %+v, %v (reacquired %d, wrote %d)", pointer, err, capacity.reacquired, len(continuations.written))
	}
	var recorded childresult.TerminalReturn
	if err := json.Unmarshal(continuations.written[0], &recorded); err != nil || recorded.ResultRef != sealed.ResultRef {
		t.Fatalf("continuation carries %+v (%v), want the sealed result", recorded, err)
	}
}

// A child bound by a control plane that predates bind-as-dispatch is still
// queued with an accepted acceptance; its finalizer records the dispatch and
// settles it. A child the engine never bound cannot be dispatched this way.
func TestFinalizeSettlesAChildBoundByAnOlderControlPlane(t *testing.T) {
	f := newProtocolFamily(t)
	if _, err := f.pool.Exec(t.Context(), `UPDATE control.child_lineage SET state='queued' WHERE instance_id=$1 AND child_id=$2`, testInstance, f.binding.Child.ChildID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE control.acceptance SET state='accepted', finished_at=NULL WHERE instance_id=$1 AND id=$2`, testInstance, f.binding.Child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.service.Finalize(t.Context(), engine.ChildFinalizeInput{Binding: f.binding, State: triggerqueue.ChildCompleted, FinishedAt: f.binding.Child.AcceptedAt.Add(time.Second)})
	if err != nil {
		t.Fatalf("finalize of an older control plane's bind: %v", err)
	}
	child, err := f.controller.GetChild(t.Context(), f.binding.Child.Identity)
	if err != nil || child.State != triggerqueue.ChildCompleted || child.ResultRef != sealed.ResultRef {
		t.Fatalf("custody = %+v, %v", child, err)
	}
	if err := f.controller.DispatchEngineChild(t.Context(), f.binding.Child.Identity); err != nil {
		t.Fatalf("a repeated dispatch of a settled child = %v, want unchanged", err)
	}

	other := newChildWorkflowFamily(t)
	started, err := other.service().StartChildWorkflow(t.Context(), other.grant.BearerToken, other.parent.pins.RunID, "never-bound", []byte(generatedChild))
	if err != nil {
		t.Fatal(err)
	}
	unbound := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: other.parent.pins.Gaggle, ParentRunID: other.parent.pins.RunID},
		StageOccurrence: other.parent.origin.StageOccurrence, InvocationKey: "never-bound"}
	if err := other.controller.DispatchEngineChild(t.Context(), unbound); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dispatch of an unbound child = %v, want refused (no engine bind)", err)
	}
	if child, err := other.controller.GetChild(t.Context(), unbound); err != nil || child.State != triggerqueue.ChildQueued || child.AcceptanceID != started.AcceptanceID {
		t.Fatalf("unbound child after a refused dispatch = %+v, %v; want unchanged", child, err)
	}
}
