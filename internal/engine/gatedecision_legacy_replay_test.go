package engine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"
)

func TestGateDecisionPreVersionSignalHistoryReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	const queue = "gate-decision-pre-version"
	worker := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	worker.RegisterWorkflowWithOptions(
		legacyGateDecisionProbe,
		workflow.RegisterOptions{Name: "gateDecisionVersionProbe"},
	)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: queue, TaskQueue: queue,
	}, "gateDecisionVersionProbe")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Client().SignalWorkflow(ctx, run.GetID(), run.GetRunID(), HumanGateSignal("approval"), "pass"); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err := run.Get(ctx, &decision); err != nil || decision != "pass" {
		t.Fatalf("legacy decision=%q err=%v", decision, err)
	}
	worker.Stop()
	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(
		gateDecisionVersionProbe,
		workflow.RegisterOptions{Name: "gateDecisionVersionProbe"},
	)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("pre-version signal history did not replay: %v", err)
	}
}

func legacyGateDecisionProbe(ctx workflow.Context) (string, error) {
	var decision string
	workflow.GetSignalChannel(ctx, HumanGateSignal("approval")).Receive(ctx, &decision)
	return decision, nil
}

func gateDecisionVersionProbe(ctx workflow.Context) (string, error) {
	if workflow.GetVersion(ctx, humanGateDecisionChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return legacyGateDecisionProbe(ctx)
	}
	return "", errors.New("new history requires the gate-decision update")
}
