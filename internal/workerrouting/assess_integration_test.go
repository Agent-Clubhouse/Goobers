//go:build integration

package workerrouting

import (
	"context"
	"io"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func routingIntegrationWorkflow(workflow.Context) error { return nil }

// The fleet assessment against a real frontend (#6020): the legacy
// DescribeTaskQueue lists versioned pollers with their deployment options
// beside unversioned ones, and the routing verdict follows set-current-version.
func TestIntegrationProbeFollowsDeploymentRouting(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Stop() }()
	c := server.Client()
	const queue = "workerrouting-integration"
	w := worker.New(c, queue, worker.Options{DeploymentOptions: worker.DeploymentOptions{
		UseVersioning:             true,
		Version:                   worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: "v2"},
		DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
	}})
	w.RegisterWorkflow(routingIntegrationWorkflow)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	target := Target{HostPort: server.FrontendHostPort(), Namespace: "default", TaskQueue: queue}

	// A versioned worker nobody promoted: polls succeed, nothing is routed.
	stalled := waitForState(t, ctx, target, StateStalled)
	if len(stalled.Remediation) != 1 || stalled.Remediation[0] != BuildCommand("v2") {
		t.Fatalf("stalled remediation = %q", stalled.Remediation)
	}

	handle := c.WorkerDeploymentClient().GetHandle(DeploymentName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := handle.SetCurrentVersion(callCtx, client.WorkerDeploymentSetCurrentVersionOptions{BuildID: "v2"})
		callCancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("set current version: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	healthy := waitForState(t, ctx, target, StateHealthy)
	if healthy.CurrentVersion != "goobers.v2" {
		t.Fatalf("healthy current version = %q", healthy.CurrentVersion)
	}
}

func waitForState(t *testing.T, ctx context.Context, target Target, want State) Assessment {
	t.Helper()
	var last Assessment
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = Probe(probeCtx, target, time.Now())
		cancel()
		if last.State == want {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("routing state = %s (%s), want %s", last.State, last.Detail, want)
	return last
}
