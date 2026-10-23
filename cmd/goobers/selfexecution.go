package main

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry"
)

func withSelfExecutionPolicy(rc runner.Config, cfg *instance.Config, tel *telemetry.Client) runner.Config {
	rc.TrustedLocalParents = trustedLocalChildren(cfg)
	rc.SelfExecutionDenied = cfg.SelfExecutionDenied()
	if rc.SelfExecutionObserved == nil {
		rc.SelfExecutionObserved = runtimewiring.SelfExecutionObserver(cfg, tel)
	}
	return rc
}
