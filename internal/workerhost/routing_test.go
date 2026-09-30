package workerhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// #5950: versioning is opt-in. A worker with a build version but no opt-in
// polls unversioned, which is what a fresh reference Temporal routes to; a
// versioned default stalls the engine on every upgrade that changes the build.
func TestWorkerOptionsUnversionedUnlessOptedIn(t *testing.T) {
	h, err := New(Config{TaskQueues: []string{"goobers-engine"}, BuildVersion: "v0.5.0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := h.workerOptions()
	if opts.DeploymentOptions.UseVersioning {
		t.Fatal("DeploymentOptions.UseVersioning = true without the engine.workerVersioning opt-in, want false")
	}
	if opts.DeploymentOptions.DefaultVersioningBehavior != 0 {
		t.Fatalf("DefaultVersioningBehavior = %v on an unversioned worker; the SDK rejects that combination", opts.DeploymentOptions.DefaultVersioningBehavior)
	}
	if !strings.Contains(opts.Identity, "goobers-worker/v0.5.0@") {
		t.Fatalf("identity %q lost the build version; it must stay diagnosable when unversioned", opts.Identity)
	}
}

func deploymentVersion(build string) *worker.WorkerDeploymentVersion {
	return &worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: build}
}

func TestRoutingProblem(t *testing.T) {
	describeErr := errors.New("worker deployment not found")
	cases := []struct {
		name      string
		versioned bool
		routing   client.WorkerDeploymentRoutingConfig
		err       error
		want      string // substring; "" means healthy
	}{
		{name: "versioned current", versioned: true, routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v2")}},
		{name: "versioned ramping", versioned: true, routing: client.WorkerDeploymentRoutingConfig{
			CurrentVersion: deploymentVersion("v1"), RampingVersion: deploymentVersion("v2"), RampingVersionPercentage: 10,
		}},
		// The #5950 observation: current stayed unversioned while the new
		// worker polled as a version. Polls succeed and receive nothing.
		{name: "versioned but current unversioned", versioned: true, routing: client.WorkerDeploymentRoutingConfig{},
			want: "current version is __unversioned__ but this worker serves goobers.v2"},
		{name: "versioned but current is previous build", versioned: true, routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v1")},
			want: "set-current-version --deployment-name goobers --build-id v2"},
		{name: "versioned, paused ramp", versioned: true, routing: client.WorkerDeploymentRoutingConfig{
			CurrentVersion: deploymentVersion("v1"), RampingVersion: deploymentVersion("v2"),
		}, want: "current version is goobers.v1"},
		{name: "versioned, describe fails", versioned: true, err: describeErr, want: "cannot verify"},
		{name: "unversioned, no deployment", err: describeErr},
		{name: "unversioned, current unversioned", routing: client.WorkerDeploymentRoutingConfig{}},
		// An instance that ran versioned and upgraded to the unversioned
		// default: the deployment still routes to its old version.
		{name: "unversioned but current versioned", routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("dev")},
			want: "current version is goobers.dev, so this UNVERSIONED worker receives no new workflow tasks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routingProblem(tc.versioned, "v2", tc.routing, tc.err)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("routingProblem = %q, want healthy", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("routingProblem = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// The running worker reports a mismatch while it lasts and its resolution
// once, and never fails over it.
func TestRunReportsRoutingMismatchWithoutFailing(t *testing.T) {
	sink := &logSink{}
	fleet := &fakeFleet{}
	h := newTestHost(t, Config{
		TaskQueues:   []string{"goobers-engine"},
		BuildVersion: "v2",
		Versioning:   true,
		Logf:         sink.logf,
	}, fleet)
	h.routingDelay = time.Millisecond
	h.routingInterval = time.Millisecond
	var mu sync.Mutex
	calls := 0
	h.describeRouting = func(context.Context, client.Client) (client.WorkerDeploymentRoutingConfig, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 {
			return client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v1")}, nil
		}
		return client.WorkerDeploymentRoutingConfig{CurrentVersion: deploymentVersion("v2")}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 4
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := sink.snapshot()
	var mismatches, resolved int
	for _, line := range lines {
		switch {
		case strings.Contains(line, "error:") && strings.Contains(line, "current version is goobers.v1"):
			mismatches++
		case strings.Contains(line, "now routes tasks to this worker"):
			resolved++
		}
	}
	if mismatches != 2 || resolved != 1 {
		t.Fatalf("logged %d mismatch and %d resolution line(s), want 2 and 1: %q", mismatches, resolved, lines)
	}
}
