package main

import (
	"context"
	"io"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type checkpointWiringEmitter struct {
	requests []livejournal.EmitRequest
}

func (e *checkpointWiringEmitter) Emit(_ context.Context, request livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.requests = append(e.requests, request)
	return livejournal.EmitResponse{Applied: len(request.Ops)}, nil
}

func TestWorkerTranscriptCheckpointRecorderWiring(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	emitter := &checkpointWiringEmitter{}
	seams := &workerSeams{store: store, scrubber: journal.NewPatternScrubber(), checkpointEmitter: emitter}
	recorder, _ := seams.recorderFor(&gaggleSeams{runsDir: t.TempDir()}, "run-a", "gaggle-a")
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
}

func TestPodTranscriptCheckpointRecorderWiring(t *testing.T) {
	t.Setenv(dispatcher.EnvDaemonAPI, "https://journal.invalid")
	t.Setenv(dispatcher.EnvBlobEndpoint, "https://blobs.invalid")
	t.Setenv(dispatcher.EnvPodToken, "run-scoped-token")
	wiring := podExecutorWiring{Kit: &agentickit.Kit{Envelope: apiv1.InvocationEnvelope{RunID: "run-pod", Gaggle: "gaggle-pod"}},
		RunsDir: t.TempDir(), Stderr: io.Discard, Scrubber: journal.NewPatternScrubber()}
	recorder := podAgenticExecutorInput(wiring).ArtifactRecorder
	wired, ok := recorder.(checkpointPodArtifacts)
	if !ok {
		t.Fatalf("pod recorder has no checkpoint transport: %T", recorder)
	}
	if wired.Dir() != wiring.RunsDir || wired.RunID != "run-pod" || wired.Gaggle != "gaggle-pod" {
		t.Fatal("pod recorder lost staging root or run scope")
	}
	emitter, ok := wired.Emitter.(*livejournal.HTTPEmitter)
	if !ok || emitter.BaseURL != "https://journal.invalid" || emitter.Token != "run-scoped-token" {
		t.Fatal("pod checkpoint transport did not reuse journal credentials")
	}
	blobs, ok := wired.Blobs.(*dispatcher.BlobClient)
	if !ok || blobs.BaseURL != "https://blobs.invalid" || blobs.Token != "run-scoped-token" {
		t.Fatal("pod checkpoint transport did not reuse blob credentials")
	}
}
