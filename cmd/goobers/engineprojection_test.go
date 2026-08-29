package main

import (
	"context"
	"errors"
	"os"
	"testing"

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
