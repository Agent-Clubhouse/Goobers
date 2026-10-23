package engine

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// fakeSpanSource is an in-memory SpanSource keyed by digest, standing in for
// a fleet blob store in tests.
type fakeSpanSource struct {
	blobs map[string][]byte
}

func (f *fakeSpanSource) Get(_ context.Context, digest string) ([]byte, error) {
	data, ok := f.blobs[digest]
	if !ok {
		return nil, errors.New("fakeSpanSource: not found")
	}
	return data, nil
}

// transcriptResult is a successful ResultEnvelope carrying a transcript
// pointer, as internal/harness's executor produces after recording a span.
func transcriptResult(data []byte) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess,
		Transcript: &apiv1.ArtifactPointer{
			Path:      "spans/test-blob",
			Digest:    journal.Digest(data),
			Size:      int64(len(data)),
			MediaType: "application/x-ndjson",
			Integrity: apiv1.IntegrityDerived,
		},
	}
}

func singleTranscriptStageProj(t *testing.T, name string, data []byte) JournalProjection {
	t.Helper()
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	return executeForProjection(t, projectionInput(name, spec), &Activities{
		Det: &scriptedStages{results: map[string][]apiv1.ResultEnvelope{
			"implement": {transcriptResult(data)},
		}},
		Workspaces: testWorkspaces(t),
	}, false)
}

// TestStageFinishedRecordsSpanOpForTranscript is the workflow-side half of
// #2907: a stage result carrying a transcript pointer must land in the
// projection as an opSpan, deterministically from history, whether or not a
// SpanSource will later be available to adopt it.
func TestStageFinishedRecordsSpanOpForTranscript(t *testing.T) {
	data := []byte(`{"event":"prompt"}`)
	proj := singleTranscriptStageProj(t, "proj-span-op", data)

	var spanOp *JournalOp
	for i := range proj.Ops {
		if proj.Ops[i].Kind == opSpan {
			spanOp = &proj.Ops[i]
			break
		}
	}
	if spanOp == nil || spanOp.Span == nil {
		t.Fatalf("projection has no span op; ops = %+v", proj.Ops)
	}
	if spanOp.Span.Stage != "implement" {
		t.Fatalf("span op stage = %q, want implement", spanOp.Span.Stage)
	}
	if spanOp.Span.Ref.Digest != journal.Digest(data) {
		t.Fatalf("span op digest = %q, want %q", spanOp.Span.Ref.Digest, journal.Digest(data))
	}
}
