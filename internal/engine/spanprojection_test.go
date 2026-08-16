package engine

import (
	"context"
	"errors"
	"path/filepath"
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

// TestProjectRunAdoptsSpanWhenSourceConfigured is the projection-writer half:
// given a SpanSource that can supply the bytes, the projected journal gets a
// real span.recorded event whose bytes read back byte-identical.
func TestProjectRunAdoptsSpanWhenSourceConfigured(t *testing.T) {
	data := []byte(`{"event":"prompt"}`)
	proj := singleTranscriptStageProj(t, "proj-span-adopt", data)

	src := &fakeSpanSource{blobs: map[string][]byte{journal.Digest(data): data}}
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj, WithSpanSource(src))
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var span *journal.Event
	for i := range events {
		if events[i].Type == journal.EventSpanRecorded {
			span = &events[i]
		}
		if events[i].Type == journal.EventError && events[i].Error != nil && events[i].Error.Code == spanUnavailableErrorCode {
			t.Fatalf("unexpected span-unavailable error event despite a configured source: %+v", events[i])
		}
	}
	if span == nil || span.Ref == nil {
		t.Fatalf("projected journal has no span.recorded event; events = %+v", events)
	}
	got, err := rd.SpanBytes(*span.Ref)
	if err != nil {
		t.Fatalf("SpanBytes: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("adopted span bytes = %q, want %q", got, data)
	}
}

// TestProjectRunNotesUnavailableSpanWithoutFailingRun pins the default
// posture (no SpanSource configured, matching every caller until a fleet
// blob store is wired in): a transcript that cannot be adopted must not block
// the run's own projection — that would reintroduce the #2895 defect
// (a tier-3 run invisible in every product surface) for the sake of a value
// apiv1.ResultEnvelope.Transcript itself documents as diagnostic evidence.
func TestProjectRunNotesUnavailableSpanWithoutFailingRun(t *testing.T) {
	data := []byte(`{"event":"prompt"}`)
	proj := singleTranscriptStageProj(t, "proj-span-missing", data)

	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun without a span source must not fail the run: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseCompleted) {
		t.Fatalf("last event = %+v, want run.finished completed", last)
	}
	found := false
	for _, ev := range events {
		if ev.Type == journal.EventSpanRecorded {
			t.Fatalf("span.recorded event present despite no span source: %+v", ev)
		}
		if ev.Type == journal.EventError && ev.Error != nil && ev.Error.Code == spanUnavailableErrorCode {
			found = true
			if ev.Stage != "implement" {
				t.Fatalf("span_unavailable event stage = %q, want implement", ev.Stage)
			}
		}
	}
	if !found {
		t.Fatalf("no span_unavailable error event; events = %+v", events)
	}
}

// TestProjectRunRejectsSpanBytesNotMatchingDigest guards against a source
// that answers Get with the wrong bytes for a digest — treated the same as
// unavailable, never adopted as-is.
func TestProjectRunRejectsSpanBytesNotMatchingDigest(t *testing.T) {
	data := []byte(`{"event":"prompt"}`)
	proj := singleTranscriptStageProj(t, "proj-span-mismatch", data)

	src := &fakeSpanSource{blobs: map[string][]byte{journal.Digest(data): []byte("not the promised bytes")}}
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj, WithSpanSource(src))
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == journal.EventSpanRecorded {
			t.Fatalf("span.recorded event present despite a digest mismatch: %+v", ev)
		}
		if ev.Type == journal.EventError && ev.Error != nil && ev.Error.Code == spanUnavailableErrorCode {
			found = true
		}
	}
	if !found {
		t.Fatalf("no span_unavailable error event after a digest mismatch; events = %+v", events)
	}
}

// TestValidateOpRejectsMalformedSpanOp pins opSpan into the same fail-closed
// structural gate every other op kind goes through.
func TestValidateOpRejectsMalformedSpanOp(t *testing.T) {
	cases := []struct {
		name string
		op   JournalOp
	}{
		{"nil payload", JournalOp{Kind: opSpan}},
		{"no name", JournalOp{Kind: opSpan, Span: &JournalSpanOp{Ref: journal.Ref{Digest: "sha256:abc"}}}},
		{"no digest", JournalOp{Kind: opSpan, Span: &JournalSpanOp{Name: "x.transcript"}}},
		{"bad attempt class", JournalOp{Kind: opSpan, Span: &JournalSpanOp{
			Name: "x.transcript", Ref: journal.Ref{Digest: "sha256:abc"}, Class: "bogus",
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateOp(tc.op, 1, 3); !errors.Is(err, ErrUnprojectable) {
				t.Fatalf("validateOp(%+v) = %v, want ErrUnprojectable", tc.op, err)
			}
		})
	}
}
