package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gate"
)

func zeroGooberWorker(t *testing.T) (*workerSeams, *blobstore.Dir) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	withDemoNetworkNoneProbe(t, func(context.Context) error { return nil })
	if code, _, stderr := runArgs(t, "init", "--demo", "--insecure", root); code != 0 {
		t.Fatalf("init zero-goober demo: %d %s", code, stderr)
	}
	// Exercise real preflight selection instead of TestMain's harness stub.
	previous := preflightHarnesses
	preflightHarnesses = preflightAgenticHarnesses
	t.Cleanup(func() { preflightHarnesses = previous })
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seams, err := newWorkerSeams(root, store)
	if err != nil {
		t.Fatal(err)
	}
	if seams.snapshot.Load() != nil {
		t.Fatal("fixture bypassed first-use snapshot initialization")
	}
	return seams, store
}

func TestWorkerAutomatedGateInitializesZeroGooberDemo(t *testing.T) {
	seams, _ := zeroGooberWorker(t)
	activity := &engine.Activities{Auto: seams.Automated()}
	env := apiv1.InvocationEnvelope{RunID: "zero-goober-run", Gaggle: "demo", WorkflowID: "demo", TaskID: "zero-goober-run:review-verdict"}
	conf := apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "verdict", "equals": "pass"}}
	for _, verdict := range []string{"pass", "fail"} {
		inputs, err := gate.AutomatedInputs(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"verdict": verdict}})
		if err != nil {
			t.Fatal(err)
		}
		env.Inputs = inputs
		outcome, err := activity.EvaluateAutomated(context.Background(), conf, env)
		if err != nil || outcome != verdict {
			t.Fatalf("zero-goober automated verdict=%s outcome=%q err=%v", verdict, outcome, err)
		}
	}
	if snapshot := seams.snapshot.Load(); snapshot == nil || len(snapshot.set.Goobers) != 0 || snapshot.gaggles["demo"] == nil {
		t.Fatal("test did not construct the real zero-goober gaggle seams")
	}
	env.Gaggle = "missing-gaggle"
	if _, err := activity.EvaluateAutomated(context.Background(), conf, env); err == nil {
		t.Fatal("unknown gaggle accepted")
	}
	env.Gaggle, env.Goober = "demo", "missing-goober"
	if _, err := seams.Agentic().Invoke(context.Background(), env); err == nil || !strings.Contains(err.Error(), `goober "missing-goober" not found in config`) {
		t.Fatalf("missing agentic selection not refused: %v", err)
	}
}

func TestWorkerArtifactGateUsesRealZeroGooberInitializationAndBoundedStore(t *testing.T) {
	seams, store := zeroGooberWorker(t)
	// The extension registry is programmatic. Obtain its evaluator through the
	// real config/snapshot construction path, never a prebuilt fake snapshot.
	g, err := seams.forGaggle("demo")
	if err != nil {
		t.Fatal(err)
	}
	auto, ok := g.cfg.Automated.(*gate.AutomatedEvaluator)
	if !ok {
		t.Fatal("worker did not construct its ordinary evaluator")
	}
	data := []byte("bounded remote evidence")
	pointer := apiv1.ArtifactPointer{Path: "artifacts/report", Digest: apiv1.Digest(data), Size: int64(len(data)), MediaType: "text/plain"}
	if err := store.Put(context.Background(), pointer.Digest, data); err != nil {
		t.Fatal(err)
	}
	auto.ArtifactChecks = map[string]gate.ArtifactCheckFunc{"evidence": func(ctx context.Context, _ map[string]interface{}, _ map[string]string, pointers []apiv1.ContextPointer, reader artifactset.Reader) (string, error) {
		if len(pointers) != 1 {
			t.Fatalf("missing upstream pointer: %+v", pointers)
		}
		content, err := reader.ReadArtifact(ctx, *pointers[0].Artifact, 100)
		if err != nil {
			return "", err
		}
		if string(content) != string(data) {
			t.Fatalf("wrong bounded artifact content: %q", content)
		}
		return gate.OutcomePass, nil
	}}
	activity := &engine.Activities{Auto: seams.Automated()}
	env := apiv1.InvocationEnvelope{RunID: "artifact-demo", Gaggle: "demo", ContextPointers: []apiv1.ContextPointer{{Name: "review.artifact[0]", Artifact: &pointer}}}
	conf := apiv1.AutomatedGate{Check: "evidence"}
	if outcome, err := activity.EvaluateAutomated(context.Background(), conf, env); err != nil || outcome != gate.OutcomePass {
		t.Fatalf("zero-goober artifact outcome=%q err=%v", outcome, err)
	}
	if auto.OpenArtifacts != nil {
		t.Fatal("evaluation mutated the shared evaluator's artifact binding")
	}
	env.ContextPointers[0].RunID = "another-run"
	if outcome, err := activity.EvaluateAutomated(context.Background(), conf, env); err != nil || outcome != gate.OutcomeFail {
		t.Fatalf("foreign evidence accepted: outcome=%q err=%v", outcome, err)
	}
}
