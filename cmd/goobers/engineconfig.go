package main

import (
	"context"
	"os"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/instance"
)

// dialEngineTemporal is the frontend dial the one-shot engine client commands
// (engine-queues, engine-project) make. A seam beside dialWorkerSweepTemporal
// and for the same reason: what these commands do that nothing else does is
// resolve WHICH frontend, namespace, and derived queue set the CLI flags and
// the instance config add up to, and that resolution is unobservable once it
// is inside a dialed client — so without it the only testable part of either
// command is its flag-parsing prologue.
var dialEngineTemporal = func(ctx context.Context, opts client.Options) (client.Client, error) {
	return client.DialContext(ctx, opts)
}

func resolveEngineConfig(instanceRoot string) (instance.EngineConfig, error) {
	if instanceRoot != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(instanceRoot).ConfigFile())
		if err != nil {
			return instance.EngineConfig{}, err
		}
		return cfg.EffectiveEngineConfig(), nil
	}
	resolved, _, err := (&instance.Config{}).ResolveEngineConfig(os.LookupEnv)
	return resolved, err
}
