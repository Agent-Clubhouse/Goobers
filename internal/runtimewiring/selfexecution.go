package runtimewiring

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
)

// SelfExecutionObserver reports self-execution policy decisions through instance telemetry.
func SelfExecutionObserver(cfg *instance.Config, tel *telemetry.Client) func(bool) {
	cfg.StartSelfExecutionAccounting()
	tel.SelfExecutionPolicy(cfg.SelfExecutionDenied())
	return func(refused bool) { cfg.ObserveSelfExecution(refused); tel.SelfExecutionObserved(refused) }
}
