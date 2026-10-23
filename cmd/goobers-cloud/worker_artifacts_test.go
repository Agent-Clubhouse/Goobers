package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/runner"
)

func TestWorkerArtifactGateReadsFleetWithoutWorkspaceWrites(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("remote evidence")
	pointer := apiv1.ArtifactPointer{Path: "artifacts/report", Digest: apiv1.Digest(data), MediaType: "text/plain", Size: int64(len(data))}
	if err := store.Put(context.Background(), pointer.Digest, data); err != nil {
		t.Fatal(err)
	}
	called := false
	auto := &gate.AutomatedEvaluator{Checks: map[string]gate.CheckFunc{}, ArtifactChecks: map[string]gate.ArtifactCheckFunc{"evidence": func(ctx context.Context, _ map[string]interface{}, _ map[string]string, pointers []apiv1.ContextPointer, reader artifactset.Reader) (string, error) {
		called = true
		if len(pointers) != 1 {
			t.Fatalf("pointers: %+v", pointers)
		}
		got, err := reader.ReadArtifact(ctx, *pointers[0].Artifact, 100)
		if err != nil {
			return "", err
		}
		if string(got) != string(data) {
			t.Fatalf("wrong remote bytes: %q", got)
		}
		return gate.OutcomePass, nil
	}}}
	runsDir := filepath.Join(t.TempDir(), "must-not-create-runs")
	seams := &workerSeams{store: store}
	seams.snapshot.Store(&workerConfigSnapshot{gaggles: map[string]*builtGaggleSeams{"web": {seams: &gaggleSeams{cfg: runner.Config{Automated: auto}, runsDir: runsDir}}}})
	conf := apiv1.AutomatedGate{Check: "evidence"}
	env := apiv1.InvocationEnvelope{RunID: "remote-run", Gaggle: "web", ContextPointers: []apiv1.ContextPointer{{Name: "producer.artifact[0]", Artifact: &pointer}}}
	got, err := seams.Automated().Evaluate(context.Background(), conf, env)
	if err != nil || got != gate.OutcomePass || !called {
		t.Fatalf("check: %s, %v, called=%v", got, err, called)
	}
	if _, err := os.Stat(runsDir); !os.IsNotExist(err) {
		t.Fatalf("artifact gate wrote local run/cache: %v", err)
	}
	if auto.OpenArtifacts != nil {
		t.Fatal("worker mutated shared evaluator")
	}
	env.ContextPointers[0].RunID = "other-run"
	if got, err := seams.Automated().Evaluate(context.Background(), conf, env); err != nil || got != gate.OutcomeFail {
		t.Fatalf("cross-run pointer not refused: %s, %v", got, err)
	}
}
