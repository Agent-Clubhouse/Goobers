package livejournal

import (
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

func (run *liveRun) childOriginReceipt(runID string, op Op) (ChildOriginReceipt, error) {
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
