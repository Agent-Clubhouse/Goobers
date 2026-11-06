package main

import (
	"log/slog"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

// newDecisionShadowObserver returns an advisory harness observer when the
// instance opted in to decisionGate shadow mode, else nil. A misconfigured or
// unreachable gate degrades to no observer: shadow mode must never stop a run.
func newDecisionShadowObserver(cfg *instance.Config, log *slog.Logger) harness.Observer {
	if cfg == nil || cfg.DecisionGate.EffectiveMode() != decisiongate.ModeShadow {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	gate, err := cfg.DecisionGate.Resolve(nil, nil)
	if err != nil {
		log.Warn("decisionGate disabled", "error", err.Error())
		return nil
	}
	observer, _ := runtimewiring.DecisionShadowObserver(gate, cfg.DecisionGate.ShadowSample, log)
	return observer
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
