package main

import "github.com/goobers/goobers/internal/instance"

// trustedLocalChildren is determined entirely by the resolved instance. An
// unavailable distributed runner never authorizes execution on the daemon host.
func trustedLocalChildren(cfg *instance.Config) bool {
	if cfg == nil || cfg.EngineProjectionEnabled() {
		return false
	}
	runners := cfg.ResolvedRunners()
	if len(runners) == 0 {
		return false
	}
	for _, runner := range runners {
		kind, err := instance.ClassifyRunnerHost(runner.Host)
		if err != nil || kind != instance.RunnerHostSelf {
			return false
		}
	}
	return true
}
