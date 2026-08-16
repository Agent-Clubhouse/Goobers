package main

import (
	"os"

	"github.com/goobers/goobers/internal/instance"
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
