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

func startRoutingWorker(t *testing.T, c client.Client, queue, build string) {
	t.Helper()
	w := worker.New(c, queue, worker.Options{DeploymentOptions: worker.DeploymentOptions{
		UseVersioning:             true,
		Version:                   worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: build},
		DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
	}})
	w.RegisterWorkflow(routingIntegrationWorkflow)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
}

func setCurrent(t *testing.T, ctx context.Context, c client.Client, build string) {
	t.Helper()
	handle := c.WorkerDeploymentClient().GetHandle(DeploymentName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := handle.SetCurrentVersion(callCtx, client.WorkerDeploymentSetCurrentVersionOptions{BuildID: build})
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("set current version %s: %v", build, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The fleet assessment against a real frontend (#6020) through a whole
// versioned rollout:
//   - the legacy DescribeTaskQueue lists versioned pollers with their
//     deployment options;
//   - a build nobody promoted stalls the engine;
//   - a second build beside a serving one is degraded with the promote command;
//   - once the second build is current, the deployment reports the replaced
//     build as draining, which is healthy, with no command reverting to it.
func TestIntegrationProbeFollowsVersionedRollout(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Stop() }()
	c := server.Client()
	const queue = "workerrouting-integration"
	target := Target{HostPort: server.FrontendHostPort(), Namespace: "default", TaskQueue: queue}

	startRoutingWorker(t, c, queue, "v1")
	stalled := waitForState(t, ctx, target, StateStalled, nil)
	if len(stalled.Remediation) != 1 || stalled.Remediation[0] != BuildCommand("v1") {
		t.Fatalf("stalled remediation = %q", stalled.Remediation)
	}
	setCurrent(t, ctx, c, "v1")
	waitForState(t, ctx, target, StateHealthy, nil)

	startRoutingWorker(t, c, queue, "v2")
	degraded := waitForState(t, ctx, target, StateDegraded, nil)
	if len(degraded.Remediation) != 1 || degraded.Remediation[0] != BuildCommand("v2") {
		t.Fatalf("unpromoted rollout remediation = %q", degraded.Remediation)
	}

	setCurrent(t, ctx, c, "v2")
	healthy := waitForState(t, ctx, target, StateHealthy, func(a Assessment) bool {
		return len(a.Workers) == 2 && a.Workers[0].Version == "goobers.v1" && a.Workers[0].Routing == WorkerDraining
	})
	if healthy.CurrentVersion != "goobers.v2" || len(healthy.Remediation) != 0 {
		t.Fatalf("after promotion: %+v", healthy)
	}
}

func waitForState(t *testing.T, ctx context.Context, target Target, want State, also func(Assessment) bool) Assessment {
	t.Helper()
	var last Assessment
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = Probe(probeCtx, target, time.Now())
		cancel()
		if last.State == want && (also == nil || also(last)) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("routing = %s (%s, workers %+v), want %s", last.State, last.Detail, last.Workers, want)
	return last
}
