package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestWorkerTranscriptCheckpointRecorderWiring(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	emitter := &checkpointWiringEmitter{}
	seams := &workerSeams{store: store, scrubber: journal.NewPatternScrubber(), checkpointEmitter: emitter}
	recorder, _ := seams.recorderFor(t.Context(), &gaggleSeams{runsDir: t.TempDir()}, "run-a", "gaggle-a")
	wired, ok := recorder.(*checkpointWorkerArtifacts)
	if !ok {
		t.Fatalf("worker recorder has no checkpoint transport: %T", recorder)
	}
	if wired.Dir() == "" {
		t.Fatal("wrapper lost the artifact context resolver")
	}
	session, err := wired.OpenTranscriptCheckpoint("build", "copilot-cli.transcript", journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("progress\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	if len(emitter.requests) != 2 {
		t.Fatalf("got %d requests, want open and append", len(emitter.requests))
	}
	for _, request := range emitter.requests {
		if request.RunID != "run-a" || request.Gaggle != "gaggle-a" || request.Ops[0].Checkpoint.Stage != "build" {
			t.Fatalf("checkpoint escaped invocation scope: %+v", request)
		}
	}
	seams.store = nil
	withoutBlobs, _ := seams.recorderFor(t.Context(), &gaggleSeams{runsDir: t.TempDir()}, "run-b", "gaggle-b")
	inline, ok := withoutBlobs.(*checkpointWorkerArtifacts)
	if !ok || inline.Blobs != nil {
		t.Fatal("worker without a blob store lost the inline checkpoint transport")
	}
}

type checkpointWiringEmitter struct {
	requests []livejournal.EmitRequest
}

func (e *checkpointWiringEmitter) Emit(_ context.Context, request livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.requests = append(e.requests, request)
	return livejournal.EmitResponse{Applied: len(request.Ops)}, nil
}

func TestWorkerRecorderReportsTrustedReceipts(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		seams := &workerSeams{scrubber: journal.NewPatternScrubber()}
		if checkpoint {
			seams.checkpointEmitter = &checkpointWiringEmitter{}
		}
		var receipts invoke.ArtifactReceipts
		ctx := invoke.WithArtifactReporter(t.Context(), receipts.Record)
		recorder, _ := seams.recorderFor(ctx, &gaggleSeams{runsDir: t.TempDir()}, "run-a", "web")
		ref, err := recorder.RecordArtifact("logical-result.json", []byte("result"))
		if err != nil {
			t.Fatal(err)
		}
		got := receipts.Snapshot()
		if len(got) != 1 || got[0].Name != "logical-result.json" || got[0].Ref.Digest != ref.Digest {
			t.Fatalf("checkpoint=%v receipts=%+v", checkpoint, got)
		}
	}
}
