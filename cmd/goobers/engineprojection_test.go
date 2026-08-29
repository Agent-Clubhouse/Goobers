package main

import (
	"context"
	"errors"
	"os"
	"testing"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/instance"
)

// noDaemonEngineClient asserts that cfg builds no shared Temporal client at
// all — the dial seam fails the test if it is reached — and returns the nil
// client the daemon would thread into every engine consumer.
func noDaemonEngineClient(t *testing.T, cfg *instance.Config) *daemonEngineClient {
	t.Helper()
	previousDial := dialDaemonEngine
	dialDaemonEngine = func(string, string) (client.Client, error) {
		return nil, errors.New("daemon dialed Temporal unexpectedly")
	}
	t.Cleanup(func() { dialDaemonEngine = previousDial })

	engineClient, err := newDaemonEngineClient(cfg)
	if err != nil {
		t.Fatalf("newDaemonEngineClient: %v", err)
	}
	if engineClient != nil {
		t.Fatal("newDaemonEngineClient built a client for an instance with no engine configuration")
	}
	return engineClient
}

func TestEngineProjectionIsInertWithoutTemporalConfiguration(t *testing.T) {
	root := t.TempDir()
	cfg := &instance.Config{}
	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), cfg, nil, noDaemonEngineClient(t, cfg), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
}

func TestEngineProjectionIsInertWithNamespaceAndTaskQueueOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv(instance.TemporalNamespaceEnv, "production")
	t.Setenv(instance.TaskQueueEnv, "production")
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte("apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), cfg, nil, noDaemonEngineClient(t, cfg), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
}

// countingTemporalClient is a client.Client that answers the only call the
// projection reconciler makes on its first tick and inherits panics for
// everything else, so a consumer that starts using the shared connection for
// something new has to say so here.
type countingTemporalClient struct {
	client.Client
	closes int
}

func (c *countingTemporalClient) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return &workflowservice.ListWorkflowExecutionsResponse{}, nil
}

func (c *countingTemporalClient) Close() { c.closes++ }

// TestDaemonDialsTemporalOnceForEveryEngineConsumer is decision 003 step 1(e)'s
// headline claim, asserted rather than restated: the projection reconciler,
// the DS6 claim-liveness probe and the engine-driven run guards share ONE
// connection to the frontend. Each used to dial its own — three TCP
// connections, three failure modes, three places an operator has to look — and
// nothing but this test stops a future change from re-dialing inside any of
// the three, because each consumer still builds fine from its own dial.
//
// The dial seam is the whole surface: dialDaemonEngine is the only entry
// point, so counting it counts connections.
func TestDaemonDialsTemporalOnceForEveryEngineConsumer(t *testing.T) {
	root := initDeterministicDemo(t)
	configureEngineInstance(t, root)
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}

	var dials int
	shared := &countingTemporalClient{}
	previousDial := dialDaemonEngine
	dialDaemonEngine = func(string, string) (client.Client, error) {
		dials++
		return shared, nil
	}
	t.Cleanup(func() { dialDaemonEngine = previousDial })

	engineClient, err := newDaemonEngineClient(cfg)
	if err != nil {
		t.Fatalf("newDaemonEngineClient: %v", err)
	}
	if engineClient == nil {
		t.Fatal("engine-configured instance built no shared Temporal client")
	}

	// Consumer 1: the completed-run projection reconciler.
	stop, err := startEngineProjection(context.Background(), l, cfg, set, engineClient, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()

	// Consumer 2: the DS6 claim-liveness probe.
	probe, closeProbe, err := buildClaimLivenessProbe(cfg, engineClient, func() []string { return nil })
	if err != nil {
		t.Fatalf("buildClaimLivenessProbe: %v", err)
	}
	if probe == nil {
		t.Fatal("engine-configured instance built no claim liveness probe")
	}
	closeProbe()

	// Consumer 3: the engine-driven run guards.
	if engineClient.Guards() == nil {
		t.Fatal("engine-configured instance built no run guards")
	}

	if dials != 1 {
		t.Fatalf("dialDaemonEngine called %d times, want exactly 1 — the three engine consumers must share the daemon's one connection", dials)
	}
	// A consumer never closes what it borrows; `goobers up` owns the lifetime.
	if shared.closes != 0 {
		t.Fatalf("shared client closed %d times by its consumers, want 0 — only the daemon that dialed it may close it", shared.closes)
	}
	engineClient.Close()
	if shared.closes != 1 {
		t.Fatalf("shared client closed %d times by its owner, want 1", shared.closes)
	}
}
