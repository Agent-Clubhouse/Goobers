package main

import (
	"fmt"
	"os"

	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporalcodec"
)

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

// resolveWorkerTemporalConfig reads the instance once so transport and payload
// settings describe the same configuration snapshot. Rootless workers retain
// the engine environment overrides and exact default data converter.
func resolveWorkerTemporalConfig(root string) (instance.EngineConfig, converter.DataConverter, error) {
	var cfg *instance.Config
	var engineConfig instance.EngineConfig
	var err error
	if root != "" {
		cfg, err = instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return engineConfig, nil, err
		}
		engineConfig = cfg.EffectiveEngineConfig()
	} else {
		engineConfig, err = resolveEngineConfig("")
		if err != nil {
			return engineConfig, nil, err
		}
	}
	dc, err := temporalcodec.DataConverter(cfg)
	if err != nil {
		return engineConfig, nil, fmt.Errorf("temporal payload codec: %w", err)
	}
	return engineConfig, dc, nil
}
