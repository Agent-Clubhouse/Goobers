package harness

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

type transcriptCheckpointRecorder interface {
	OpenTranscriptCheckpoint(stage, name string, scrubber journal.Scrubber) (journal.TranscriptCheckpointSession, error)
}

func (e *Executor) beginTranscriptCapture(stage string, req *RunRequest) (journal.TranscriptCheckpointSession, error) {
	recorder, ok := e.recorder.(transcriptCheckpointRecorder)
	if !ok {
		return nil, nil // Other recorder transports need their own durable path.
	}
	capture, err := recorder.OpenTranscriptCheckpoint(stage, e.adapter.Name()+".transcript", e.scrubber)
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

func (e *Executor) runAdapter(ctx context.Context, req RunRequest, nested NestedPolicyCapability) (Outcome, error) {
	if nested != nil {
		return nested.RunNested(ctx, req)
	}
	return e.adapter.Run(ctx, req)
}

func (e *Executor) recordFinalTranscript(ctx context.Context, capture journal.TranscriptCheckpointSession, runErr error, stage, name, schema string, data []byte) (journal.Ref, error) {
	if capture != nil && runErr == nil && ctx.Err() == nil {
		return capture.RecordFinal(schema, data)
	}
	return e.recorder.RecordSpanWithSchema(stage, name, schema, data)
}
