package main

// Review probe for #7857 (reviewer-2). Copy into cmd/goobers-cloud and run:
//   go test -race -run TestWorker ./cmd/goobers-cloud
// Before the move, `goobers worker` built executors through cmd/goobers
// buildRunnerConfig, whose runnerExecutionFence wrapped every invocation in
// withCurrentMergeAuthority (SEC-053): bind executor.WithConfigDirectory to the
// admitted tree, then refuse a pinned invocation whose merge capability the
// current operator tree no longer grants, BEFORE the claim fence or executor.

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

func TestWorkerFactoriesKeepMergeRevocationAndPinnedConfigDir(t *testing.T) {
	root := initDemo(t)
	seams := workerReloadSeams(t, root)
	var reached int
	var boundDir string
	seams.executionFence = func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		reached++
		boundDir = executor.ConfigDirectoryFromContext(ctx)
		return ctx, func() {}, errors.New("probe: claim fence reached")
	}
	previous := runtimewiring.NewAgenticAdapter
	runtimewiring.NewAgenticAdapter = func(string, map[string]string) harness.Adapter { return &harness.FakeAdapter{} }
	t.Cleanup(func() { runtimewiring.NewAgenticAdapter = previous })
	gaggle, err := seams.forGaggle(pinGaggle)
	if err != nil {
		t.Fatal(err)
	}
	recorder, registrar := seams.recorderFor(t.Context(), gaggle, "pinned-run", pinGaggle)
	deterministic, err := gaggle.cfg.NewDeterministic(recorder, registrar)
	if err != nil {
		t.Fatal(err)
	}
	// The demo's current tree grants no merge capability to any stage, so a
	// pinned invocation still carrying github:pr:merge has been revoked.
	env := apiv1.InvocationEnvelope{
		RunID: "pinned-run", TaskID: "pinned-run:implement", Gaggle: pinGaggle, WorkflowID: pinWorkflow, Goober: pinGoober,
		ConfigGeneration: "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Capabilities:     []string{string(capability.GitHubPRMerge)},
	}

	agentic, err := gaggle.cfg.NewAgentic(pinGoober, recorder, registrar)
	if err != nil {
		t.Fatal(err)
	}
	factories := map[string]func(apiv1.InvocationEnvelope) error{
		"deterministic": func(env apiv1.InvocationEnvelope) error {
			_, err := deterministic.Run(t.Context(), env, apiv1.DeterministicRun{})
			return err
		},
		"invoke": func(env apiv1.InvocationEnvelope) error { _, err := agentic.Invoke(t.Context(), env); return err },
		"review": func(env apiv1.InvocationEnvelope) error { _, err := agentic.Review(t.Context(), env); return err },
	}
	for name, invoke := range factories {
		t.Run(name, func(t *testing.T) {
			reached, boundDir = 0, ""
			if err := invoke(env); err == nil || reached != 0 {
				t.Fatalf("revoked merge authority reached the claim fence: err=%v reached=%d", err, reached)
			}
			ordinary := env
			ordinary.Capabilities = nil
			_ = invoke(ordinary)
			if want := instance.NewLayout(root).ForGaggle(pinGaggle).ConfigDir(); reached != 1 || boundDir != want {
				t.Errorf("claim fence reached=%d config directory=%q, want once with %q", reached, boundDir, want)
			}
		})
	}
}
