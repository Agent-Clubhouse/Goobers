package main

import (
	"io"
	"testing"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestPodTranscriptCheckpointRecorderWiring(t *testing.T) {
	t.Setenv(dispatcher.EnvDaemonAPI, "https://journal.invalid")
	t.Setenv(dispatcher.EnvBlobEndpoint, "https://blobs.invalid")
	t.Setenv(dispatcher.EnvPodToken, "run-scoped-token")
	t.Setenv(dispatcher.EnvRunID, "run-pod")
	t.Setenv(dispatcher.EnvGaggle, "gaggle-pod")
	wiring := runtimewiring.PodExecutorWiring{Kit: &agentickit.Kit{Envelope: apiv1.InvocationEnvelope{RunID: "run-pod"}},
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
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	inline, ok := podAgenticExecutorInput(wiring).ArtifactRecorder.(checkpointPodArtifacts)
	if !ok || inline.Blobs != nil {
		t.Fatal("pod without a blob endpoint lost checkpoints or retained a typed-nil blob writer")
	}
}
