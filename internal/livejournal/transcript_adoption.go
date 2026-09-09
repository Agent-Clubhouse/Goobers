package livejournal

import (
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// adoptCompletedTranscript joins the workflow's result pointer to a canonical
// transcript already committed by the worker. It persists the workflow retry
// key without recording the same transcript twice or fetching it from an
// optional fleet store. Reading durable history also makes this work after a
// daemon restart and for runner-owned, adopted journals.
func (run *liveRun) adoptCompletedTranscript(runID string, op Op) (bool, error) {
	span := op.Span
	if span == nil || (span.Name != "transcript" && !strings.HasSuffix(span.Name, ".transcript")) {
		return false, nil
	}
	reader, err := journal.OpenReadOnly(run.dir)
	if err != nil {
		return true, err
	}
	events, err := reader.Events()
	if err != nil {
		return true, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		capture, _ := event.Runner["transcriptCaptureComplete"].(string)
		if !event.KnownSchema() || event.Type != journal.EventSpanRecorded || !validRemoteCaptureID(capture) ||
			strings.TrimPrefix(event.Stage, runID+":") != strings.TrimPrefix(span.Stage, runID+":") ||
			event.Ref == nil || event.Ref.Digest != span.Ref.Digest || event.Ref.Size != span.Ref.Size {
			continue
		}
		if _, err := reader.ArtifactBytesBounded(*event.Ref, journal.MaxCheckpointScrubBytes); err != nil {
			return true, err // A marker alone is not proof of durable custody.
		}
		if err := run.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: span.Stage,
			Attempt: span.Attempt, AttemptClass: span.Class, Runner: map[string]any{
				EmitKeyRunnerField: op.Key, "transcriptCaptureAdopted": capture, "transcriptSeq": event.Seq,
			}}); err != nil {
			return true, err
		}
		run.keys[op.Key] = run.jr.Seq()
		return true, nil
	}
	return false, nil
}
