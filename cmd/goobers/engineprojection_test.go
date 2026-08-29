package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/instance"
)

func TestEngineProjectionIsInertWithoutTemporalConfiguration(t *testing.T) {
	root := t.TempDir()
	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), &instance.Config{}, nil, nil, nil, nil, nil, nil)
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

	previousDial := dialEngineProjection
	dialEngineProjection = func(string, string) (client.Client, error) {
		return nil, errors.New("projection dialed unexpectedly")
	}
	t.Cleanup(func() { dialEngineProjection = previousDial })

	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), cfg, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
}
