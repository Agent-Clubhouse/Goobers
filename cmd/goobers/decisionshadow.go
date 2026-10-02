package main

import (
	"log/slog"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
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
	newObs := func(outcome string) *decisiongate.Observer {
		return decisiongate.NewObserver(gate, cfg.DecisionGate.ShadowSample, 2, func(r decisiongate.ShadowRecord) {
			log.Info("decisiongate.shadow",
				"outcome", outcome,
				"verdict", string(r.Verdict), "probability", r.Probability, "confidence", r.Confidence,
				"cached", r.Cached, "agentClaimedBad", r.AgentClaimedBad, "error", errString(r.Err))
		})
	}
	// Successes are scored too so the log can show false alarms on healthy
	// replies, not only detections on failed ones.
	ok, other := newObs("success"), newObs("non-success")
	return func(env apiv1.InvocationEnvelope, result apiv1.ResultEnvelope) {
		if result.Status == apiv1.ResultSuccess {
			ok.Observe(env.RunID, result.Summary)
			return
		}
		other.Observe(env.RunID, result.Summary)
	}
}
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
