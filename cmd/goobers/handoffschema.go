package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
)

func newHandoffSchemaLoader(cfg *instance.Config, configDir string) runner.HandoffSchemaLoader {
	if cfg == nil || cfg.DecisionGate.EffectiveMode() == decisiongate.ModeOff {
		return nil
	}
	var cache sync.Map
	return func(schemaPath string) (*handoffcheck.Schema, error) {
		if strings.TrimSpace(schemaPath) == "" {
			return nil, fmt.Errorf("handoff schema path is required")
		}
		if cached, ok := cache.Load(schemaPath); ok {
			entry := cached.(handoffSchemaEntry)
			return entry.schema, entry.err
		}
		full, err := apiv1.ResolveContainedPath(configDir, schemaPath)
		if err != nil {
			err = fmt.Errorf("resolve %q: %w", schemaPath, err)
			cache.Store(schemaPath, handoffSchemaEntry{err: err})
			return nil, err
		}
		data, err := os.ReadFile(full)
		if err != nil {
			err = fmt.Errorf("read %q: %w", filepath.ToSlash(full), err)
			cache.Store(schemaPath, handoffSchemaEntry{err: err})
			return nil, err
		}
		schema, err := handoffcheck.Compile(schemaPath, "", data)
		cache.Store(schemaPath, handoffSchemaEntry{schema: schema, err: err})
		return schema, err
	}
}

type handoffSchemaEntry struct {
	schema *handoffcheck.Schema
	err    error
}
