package main

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
)

func selfExecutionObserver(cfg *instance.Config, tel *telemetry.Client) func(bool) {
	cfg.StartSelfExecutionAccounting()
	tel.SelfExecutionPolicy(cfg.SelfExecutionDenied())
	return func(refused bool) { cfg.ObserveSelfExecution(refused); tel.SelfExecutionObserved(refused) }
}

func withSelfExecutionPolicy(rc runner.Config, cfg *instance.Config, tel *telemetry.Client) runner.Config {
	rc.TrustedLocalParents = trustedLocalChildren(cfg)
	rc.SelfExecutionDenied = cfg.SelfExecutionDenied()
	rc.SelfExecutionObserved = selfExecutionObserver(cfg, tel)
	return rc
}
