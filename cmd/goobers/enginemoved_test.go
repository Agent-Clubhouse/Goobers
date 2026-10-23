package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestEngineMovedWarning(t *testing.T) {
	if warning := engineMovedWarning(&instance.Config{}); warning != "" {
		t.Fatalf("local config warning = %q", warning)
	}
	cfg := &instance.Config{Engine: &instance.EngineConfig{}}
	if warning := engineMovedWarning(cfg); !strings.Contains(warning, "goobers-cloud") {
		t.Fatalf("engine config warning = %q", warning)
	}
}
