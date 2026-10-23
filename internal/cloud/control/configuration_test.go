package control

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type countedConfiguration struct {
	reloads int
	stop    error
}

func (c *countedConfiguration) Admit(string, string, string) error { return nil }
func (c *countedConfiguration) Reload(context.Context) error       { c.reloads++; return c.stop }

func TestConfigurationPersistsAcrossEpochsAndReloadsBeforeRecovery(t *testing.T) {
	config := &countedConfiguration{stop: errors.New("stop before database access")}
	pool := &pgxpool.Pool{}
	manager := &lease.Manager{}
	ok := func(context.Context) error { return nil }
	runtime := Runtime{Configuration: config, Pool: pool, InstanceID: "instance", Lease: manager, Temporal: struct{ client.Client }{}, TaskQueue: "cloud",
		Resolve: func(context.Context, triggerqueue.Record) (engine.RunInput, error) { return engine.RunInput{}, nil },
		Services: func(context.Context, *lease.Handle) (EpochServices, error) {
			return EpochServices{
				RebuildBeforeReap: ok, ReconcileUncertain: ok, ReattachAwaiters: ok, L1: ok, L2: ok, L3: ok, Stop: ok,
				RestoreObservations: func(context.Context, []Observation) error { return nil },
				CaptureObservations: func(context.Context) ([]Observation, error) { return nil, nil },
			}, nil
		},
	}
	for epoch := int64(1); epoch <= 2; epoch++ {
		services, err := runtime.Build(t.Context(), &lease.Handle{InstanceID: "instance", Epoch: epoch})
		if err != nil {
			t.Fatal(err)
		}
		if config.reloads != int(epoch-1) {
			t.Fatal("configuration recreated or reloaded while merely constructing services")
		}
		if err := services.RebuildBeforeReap(t.Context()); !errors.Is(err, config.stop) {
			t.Fatalf("epoch %d preparation did not reload before recovery: %v", epoch, err)
		}
		if runtime.Configuration != config || config.reloads != int(epoch) {
			t.Fatal("epoch did not use process-wide configuration instance")
		}
	}
}
