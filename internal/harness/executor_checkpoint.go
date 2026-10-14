package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/diagnostics/featureusage"
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
	defer featureusage.RecordAdapter(e.recorder, e.adapter.Name(), req.Envelope.TaskID)
	ctx, window := executiondeadline.Observe(ctx, e.recorder, strings.TrimPrefix(req.Envelope.TaskID, req.Envelope.RunID+":"), req.Attempt)
	readiness := captureReadiness(&req)
	var out Outcome
	var err error
	if nested != nil {
		out, err = nested.RunNested(ctx, req)
	} else {
		out, err = e.adapter.Run(ctx, req)
	}
	// Taken the moment the adapter hands the completion back, so a claim the
	// completion makes about time is judged against when it was made (#5871).
	out.execution = executionEvidence{window: window, returned: time.Now().UTC(), readiness: readiness}
	return out, err
}

func (e *Executor) recordFinalTranscript(ctx context.Context, capture journal.TranscriptCheckpointSession, runErr error, stage, name, schema string, data []byte) (journal.Ref, error) {
	if capture != nil && runErr == nil && ctx.Err() == nil {
		return capture.RecordFinal(schema, data)
	}
	return e.recorder.RecordSpanWithSchema(stage, name, schema, data)
}
