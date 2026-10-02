package livejournal

import "github.com/goobers/goobers/internal/journal"

// StartAcknowledgment identifies a durable, controller-authenticated start. Its metadata
// comes from the accepted journal event, including on duplicate requests; a
// caller-supplied op cannot relabel another event that already owns its key.
type StartAcknowledgment struct {
	Key     string               `json:"key"`
	Seq     uint64               `json:"seq"`
	Type    journal.EventType    `json:"type"`
	Stage   string               `json:"stage"`
	Branch  int                  `json:"branch"`
	Attempt int                  `json:"attempt"`
	Class   journal.AttemptClass `json:"class,omitempty"`
}

func (run *liveRun) rememberStart(key string, ev journal.Event) {
	if ev.Type != journal.EventStageStarted && ev.Type != journal.EventReviewerStarted {
		return
	}
	proof, _ := ev.Runner[ControllerStartProofField].(string)
	if run.startAuthority == nil || !run.startAuthority.VerifyControllerStart(run.runID, key, ev, proof) {
		return
	}
	if run.starts == nil {
		run.starts = make(map[string]StartAcknowledgment)
	}
	run.starts[key] = StartAcknowledgment{Key: key, Seq: ev.Seq, Type: ev.Type, Stage: ev.Stage, Branch: ev.Branch, Attempt: ev.Attempt, Class: ev.AttemptClass}
}

func (run *liveRun) acknowledgeStart(op Op, resp *EmitResponse) {
	// Adopted handles can be appended by their local owner outside this
	// writer's lock. They need a journal Append-returning-Seq primitive before
	// this transport can promise an exact acknowledgment for them.
	if run.adopted || op.Event == nil || (op.Event.Type != journal.EventStageStarted && op.Event.Type != journal.EventReviewerStarted) {
		return
	}
	if ack, ok := run.starts[op.Key]; ok {
		resp.Starts = append(resp.Starts, ack)
	}
}
