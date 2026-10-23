package main

import (
	"testing"

	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestLocalRunnerConfigPreservesOptionalTelemetry(t *testing.T) {
	if got := localRunnerConfig(runtimewiring.ExecutorConfig{}); got.Telemetry != nil {
		t.Fatal("absent telemetry became a non-nil interface during adaptation")
	}
	client := &telemetry.Client{}
	if got := localRunnerConfig(runtimewiring.ExecutorConfig{Telemetry: client}); got.Telemetry != client {
		t.Fatal("configured telemetry client changed during adaptation")
	}
}
