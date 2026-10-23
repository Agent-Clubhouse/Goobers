package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/instance"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestWorkerExecutionWithoutBearerRefusesRemoteDaemon(t *testing.T) {
	emitter := &livejournal.HTTPEmitter{BaseURL: "https://daemon.example"}
	fence := runtimewiring.RemoteSharedExecutionFence(emitter.BaseURL, func(runID string) (string, error) { return workerExecutionBearer(emitter, runID) })
	_, stop, err := fence(t.Context(), apiv1.InvocationEnvelope{RunID: "run"})
	stop()
	if err == nil {
		t.Fatal("remote execution bypassed missing credentials")
	}
}

// Exercise the factories installed by the worker, so dropping its ExecutionFence
// input cannot leave the standalone fence tests green while stages run unfenced.
func TestWorkerExecutorFactoriesEnforceExecutionFence(t *testing.T) {
	seams := workerReloadSeams(t, initDemo(t))
	refused := errors.New("fixture claim is no longer owned")
	var checked, stopped int
	seams.executionFence = func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		if env.RunID != "fenced-run" {
			t.Fatalf("fenced wrong invocation: %+v", env)
		}
		checked++
		return ctx, func() { stopped++ }, refused
	}
	previous := runtimewiring.NewAgenticAdapter
	runtimewiring.NewAgenticAdapter = func(string, map[string]string) harness.Adapter { return &harness.FakeAdapter{} }
	t.Cleanup(func() { runtimewiring.NewAgenticAdapter = previous })
	gaggle, err := seams.forGaggle(pinGaggle)
	if err != nil {
		t.Fatal(err)
	}
	recorder, registrar := seams.recorderFor(gaggle, "fenced-run", pinGaggle)
	deterministic, err := gaggle.cfg.NewDeterministic(recorder, registrar)
	if err != nil {
		t.Fatal(err)
	}
	agentic, err := gaggle.cfg.NewAgentic(pinGoober, recorder, registrar)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{RunID: "fenced-run", Gaggle: pinGaggle, Goober: pinGoober}
	if _, err := deterministic.Run(t.Context(), env, apiv1.DeterministicRun{}); !errors.Is(err, refused) {
		t.Fatalf("deterministic bypassed fence: %v", err)
	}
	if _, err := agentic.Invoke(t.Context(), env); !errors.Is(err, refused) {
		t.Fatalf("agentic invoke bypassed fence: %v", err)
	}
	if _, err := agentic.Review(t.Context(), env); !errors.Is(err, refused) {
		t.Fatalf("agentic review bypassed fence: %v", err)
	}
	if checked != 3 || stopped != 3 {
		t.Fatalf("checks=%d stops=%d, want 3 each", checked, stopped)
	}
}

func TestRootOnlyWorkerRefusesSharedClaims(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	fence := localSharedExecutionFence(layout)
	_, stop, err := fence(t.Context(), apiv1.InvocationEnvelope{RunID: "root-only"})
	stop()
	if err != nil {
		t.Fatalf("local claims refused: %v", err)
	}
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", pinGaggle, "workflows", pinWorkflow+".yaml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	workflow.Spec.Readiness.ClaimVisibility = "shared"
	data, err = yaml.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, stop, err = fence(t.Context(), apiv1.InvocationEnvelope{RunID: "root-only"})
	stop()
	if err == nil || !strings.Contains(err.Error(), "ownership observer") {
		t.Fatalf("shared claims bypassed ownership observation: %v", err)
	}
}
