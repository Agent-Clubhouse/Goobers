package livejournal

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ChildStageStart asks the trusted journal writer to bind a stage origin at
// commit. Nil preserves ordinary append behavior. It is not an agent assertion
// of authority; workload principals must not submit this operation.
type ChildStageStart struct {
	Continuation bool `json:"continuation,omitempty"`
}

// ChildOriginReceipt identifies one committed stage-start op, not a batch's
// sequence watermark. The writer returns identical receipts after dedup/recovery.
type ChildOriginReceipt struct {
	Key     string                    `json:"key"`
	Seq     uint64                    `json:"seq"`
	Branch  int                       `json:"branch"`
	Stage   string                    `json:"stage"`
	Attempt int                       `json:"attempt"`
	Class   journal.AttemptClass      `json:"attemptClass,omitempty"`
	Origin  apiv1.ChildWorkflowOrigin `json:"origin"`
}

func validateChildStart(op Op) error {
	// These fields are writer-owned even without an explicit binding request.
	// Otherwise an ordinary append could forge a receipt recovered after restart.
	if op.Event != nil {
		if _, present := op.Event.Runner[journal.ChildWorkflowOccurrenceKey]; present {
			return errors.New("child occurrence must be assigned by the journal writer")
		}
		if _, present := op.Event.Runner[journal.ChildWorkflowAttemptKey]; present {
			return errors.New("child attempt must be assigned by the journal writer")
		}
	}
	if op.ChildStageStart == nil {
		return nil
	}
	if op.Kind != OpAppend || op.Event == nil || op.Event.Type != journal.EventStageStarted || op.Event.Stage == "" || op.Event.Attempt < 1 {
		return errors.New("child origin binding requires a stage-start append")
	}
	return nil
}

// Validate checks a receipt against its requested op and actual sequence. It
// never derives authority from a projection ordinal or the batch high-water mark.
func (r ChildOriginReceipt) Validate(runID string, op Op) error {
	if op.ChildStageStart == nil || validateChildStart(op) != nil || r.Key != op.Key || r.Seq == 0 || r.Branch != op.Event.Branch || r.Stage != op.Event.Stage || r.Attempt != op.Event.Attempt || r.Class != op.Event.AttemptClass {
		return errors.New("child origin receipt differs from the requested stage start")
	}
	event := journal.Event{Type: journal.EventStageStarted, Seq: r.Seq, Branch: r.Branch, Stage: r.Stage, Attempt: r.Attempt,
		Runner: map[string]any{journal.ChildWorkflowOccurrenceKey: r.Origin.StageOccurrence, journal.ChildWorkflowAttemptKey: r.Origin.AttemptID}}
	if _, err := journal.ChildWorkflowOriginForEvent(runID, event); err != nil {
		return err
	}
	if op.ChildStageStart.Continuation != (r.Origin.StageOccurrence != r.Origin.AttemptID) {
		return errors.New("child origin receipt differs from the requested continuation")
	}
	return nil
}

func (run *liveRun) rememberChildStart(key string, event journal.Event) {
	if event.Type != journal.EventStageStarted {
		return
	}
	if _, bound := event.Runner[journal.ChildWorkflowOccurrenceKey]; !bound {
		return
	}
	if run.childStarts == nil {
		run.childStarts = make(map[string]journal.Event)
	}
	run.childStarts[key] = event
}

// appendEventWithChildOrigin preserves committed append identity before the
// caller notifies observers. Caller holds run.mu and has attached the emit key.
func (w *Writer) appendEventWithChildOrigin(run *liveRun, op Op, event *journal.Event) error {
	if op.ChildStageStart == nil {
		if err := w.appendRun(run, *event); err != nil {
			return err
		}
		run.keys[op.Key] = w.seq(run)
		return nil
	}
	var (
		seq    uint64
		origin *apiv1.ChildWorkflowOrigin
		err    error
	)
	if run.adopted {
		seq, origin, err = run.jr.AppendChildStageStarted(*event, op.ChildStageStart.Continuation)
	} else {
		seq, origin, err = w.backend.AppendChildStageStarted(context.Background(), run.handle, run.owner, journal.ChildStageStartRecord{
			Event: *event, Continuation: op.ChildStageStart.Continuation,
		})
	}
	if err != nil {
		return err
	}
	event.Seq = seq
	event.Runner[journal.ChildWorkflowOccurrenceKey] = origin.StageOccurrence
	event.Runner[journal.ChildWorkflowAttemptKey] = origin.AttemptID
	if _, present := event.Runner["artifactVisit"]; present {
		event.Runner["artifactVisit"] = seq
	}
	run.keys[op.Key] = seq
	run.rememberChildStart(op.Key, *event)
	return nil
}

func (w *Writer) childOriginReceipt(run *liveRun, runID string, op Op) (ChildOriginReceipt, error) {
	// Recovery can retain a complete append whose checkpoint failed. Never
	// expose its receipt until the checkpoint has also succeeded, even after
	// a process restart or an idle close discarded the in-memory error state.
	if w.hasRun(run) {
		var err error
		if run.adopted {
			err = run.jr.Checkpoint()
		} else {
			err = w.backend.Checkpoint(context.Background(), run.handle, run.owner)
		}
		if err != nil {
			return ChildOriginReceipt{}, err
		}
	}
	event, ok := run.childStarts[op.Key]
	if !ok {
		return ChildOriginReceipt{}, fmt.Errorf("no committed child origin for emit key %q", op.Key)
	}
	origin, err := journal.ChildWorkflowOriginForEvent(runID, event)
	if err != nil {
		return ChildOriginReceipt{}, err
	}
	receipt := ChildOriginReceipt{Key: op.Key, Seq: event.Seq, Branch: event.Branch, Stage: event.Stage, Attempt: event.Attempt, Class: event.AttemptClass, Origin: *origin}
	return receipt, receipt.Validate(runID, op)
}

// invalidateChildAppend discards uncertain cached authority. Owned writers are
// closed so the next acquire reconstructs dedup keys from committed events.
// A loan cannot be closed here; quarantine it until reconciliation succeeds.
// Caller holds run.mu, including across removal from the writer cache.
func (w *Writer) invalidateChildAppend(runID string, run *liveRun) {
	if run.adopted && run.jr != nil && !run.jr.Closed() {
		run.childAppendUncertain = true
		return
	}
	if w.hasRun(run) {
		_ = w.retire(run)
		w.clearRun(run)
	}
	// Clear terminal dedup too, so already-acquired stale references must
	// reacquire instead of serving receipts from the uncertain cache.
	run.keys = make(map[string]uint64)
	run.childStarts = nil
	w.forgetLoan(runID, run)
}

// reconcileChildAppend repairs the borrowed handle without releasing its
// owner's lock. Nothing can use cached dedup or return receipts until both
// the append boundary and checkpoint are repaired and the durable keys read.
func (w *Writer) reconcileChildAppend(run *liveRun) error {
	var err error
	if run.adopted {
		err = run.jr.RepairAppendBoundary()
	} else {
		err = w.backend.RepairAppendBoundary(context.Background(), run.handle, run.owner)
	}
	if err != nil {
		return err
	}
	if run.adopted {
		err = run.jr.Checkpoint()
	} else {
		err = w.backend.Checkpoint(context.Background(), run.handle, run.owner)
	}
	if err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(run.dir)
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	deriveDedupState(run, events)
	run.childAppendUncertain = false
	return nil
}
