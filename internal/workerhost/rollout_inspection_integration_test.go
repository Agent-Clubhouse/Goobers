//go:build integration

package workerhost

import (
	"context"
	"io"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workerrouting"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This is a real local dev-server qualification of the read surface. Its native
// mutations only arrange the fixture; no product rollout mutation is introduced.
func TestIntegrationTemporalRolloutInspection(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
		ExtraArgs: []string{"--dynamic-config-value", "frontend.enableWorkerVersioning=true", "--dynamic-config-value", "system.enableWorkerVersioning=true",
			"--dynamic-config-value", "frontend.workerVersioningDataAPIs=true", "--dynamic-config-value", "frontend.workerVersioningRuleAPIs=true"}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	oldWorker := startVersionedWorker(t, server.Client(), "rollout-inspection", "old", "old-inspector", &versioningRecorder{})
	defer oldWorker.Stop()
	newWorker := startVersionedWorker(t, server.Client(), "rollout-inspection", "candidate", "candidate-inspector", &versioningRecorder{})
	defer newWorker.Stop()
	handle := server.Client().WorkerDeploymentClient().GetHandle(DeploymentName)
	waitForCurrentVersion(t, ctx, handle, "old")
	// Readiness here means the native service has registered the new version;
	// it is not represented as a production health proof by the command.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err = handle.SetRampingVersion(ctx, client.WorkerDeploymentSetRampingVersionOptions{BuildID: "candidate", Percentage: 15})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture ramp registration: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertState := func(phase string, percent float32) {
		t.Helper()
		got, err := workerrouting.ReadRollout(ctx, handle, time.Now())
		if err != nil || got.Phase != phase || got.RampPercentage != percent || len(got.Versions) != 2 {
			t.Fatalf("snapshot = %+v, err %v", got, err)
		}
	}
	assertState("ramping", 15)
	if _, err := handle.SetRampingVersion(ctx, client.WorkerDeploymentSetRampingVersionOptions{BuildID: "candidate", Percentage: 0}); err != nil {
		t.Fatal(err)
	}
	assertState("paused", 0)
	waitForCurrentVersion(t, ctx, handle, "candidate")
	assertState("stable", 0)
}
