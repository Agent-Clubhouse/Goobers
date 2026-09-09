package harness

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

type transcriptCheckpointRecorder interface {
	BeginTranscriptCaptureWithScrubber(stage, name string, scrubber journal.Scrubber) (*journal.TranscriptCapture, error)
}

func (e *Executor) beginTranscriptCapture(stage string, req *RunRequest) (*journal.TranscriptCapture, error) {
	recorder, ok := e.recorder.(transcriptCheckpointRecorder)
	if !ok {
		return nil, nil // Other recorder transports need their own durable path.
	}
	capture, err := recorder.BeginTranscriptCaptureWithScrubber(stage, e.adapter.Name()+".transcript", e.scrubber)
	if err != nil {
		return nil, fmt.Errorf("harness: begin transcript checkpoints: %w", err)
	}
	req.TranscriptCheckpoint = func(delta InvocationTranscriptDelta) error {
		return capture.Append(journal.TranscriptCheckpoint{
			Stream: fmt.Sprintf("%s/%d", delta.Source, delta.Invocation),
			Offset: delta.Offset, Data: delta.Data, DroppedBytes: delta.DroppedBytes, Reason: delta.Reason,
		})
	}
	return capture, nil
}

func (e *Executor) recordFinalTranscript(capture *journal.TranscriptCapture, completed bool, stage, name, schema string, data []byte) (journal.Ref, error) {
	if capture != nil && completed {
		return capture.RecordFinal(schema, data)
	}
	return e.recorder.RecordSpanWithSchema(stage, name, schema, data)
}
