package main

import (
	"log/slog"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
)

// newDecisionShadowObserver returns an advisory harness observer when the
// instance opted in to decisionGate shadow mode, else nil. A misconfigured or
// unreachable gate degrades to no observer: shadow mode must never stop a run.
func newDecisionShadowObserver(cfg *instance.Config, log *slog.Logger) harness.Observer {
	if cfg == nil {
		return nil
	}
	observer, _ := newDecisionShadowObserverFromSettings(cfg.DecisionGate, os.Getenv, log)
	return observer
}

func newDecisionShadowObserverFromSettings(settings *decisiongate.Settings, getenv func(string) string, log *slog.Logger) (harness.Observer, func()) {
	if settings.EffectiveMode() != decisiongate.ModeShadow {
		return nil, func() {}
	}
	if log == nil {
		log = slog.Default()
	}
	gate, err := settings.Resolve(getenv, nil)
	if err != nil {
		log.Warn("decisionGate disabled", "error", err.Error())
		return nil, func() {}
	}
	newObs := func(outcome string) *decisiongate.Observer {
		return decisiongate.NewObserver(gate, settings.ShadowSample, 2, func(r decisiongate.ShadowRecord) {
			log.Info("decisiongate.shadow",
				"outcome", outcome,
				"inputValid", shadowInputValidity(r),
				"verdict", string(r.Verdict), "probability", r.Probability, "confidence", r.Confidence,
				"cached", r.Cached, "agentClaimedBad", r.AgentClaimedBad, "error", errString(r.Err))
		})
	}
	// Successes are scored too so the log can show false alarms on healthy
	// replies, not only detections on failed ones.
	ok, other := newObs("success"), newObs("non-success")
	observe := func(env apiv1.InvocationEnvelope, result apiv1.ResultEnvelope) {
		if inputValid, known := decisionShadowInputValidity(result); known {
			if result.Status == apiv1.ResultSuccess {
				ok.ObserveValidated(env.RunID, inputValid, result.Summary)
				return
			}
			other.ObserveValidated(env.RunID, inputValid, result.Summary)
			return
		}
		if result.Status == apiv1.ResultSuccess {
			ok.Observe(env.RunID, result.Summary)
			return
		}
		other.Observe(env.RunID, result.Summary)
	}
	return observe, func() {
		ok.Wait()
		other.Wait()
	}
}

func decisionGateCredentialSources(settings *decisiongate.Settings) ([]credentials.TokenRef, []credentials.Grant) {
	if settings.EffectiveMode() != decisiongate.ModeShadow {
		return nil, nil
	}
	refs := []credentials.TokenRef{
		{Name: decisiongate.CredentialBaseURL, Env: settings.BaseURLEnv},
		{Name: decisiongate.CredentialAPIKey, Env: settings.KeyEnv},
		{Name: decisiongate.CredentialModel, Env: settings.ModelEnv},
	}
	grants := make([]credentials.Grant, len(refs))
	for i := range refs {
		grants[i] = credentials.Grant{Capability: refs[i].Name, Ref: refs[i].Name}
	}
	return refs, grants
}

func decisionShadowInputValidity(result apiv1.ResultEnvelope) (bool, bool) {
	report, ok := handoffcheck.ReportFromOutputs(result.Outputs)
	if !ok {
		return false, false
	}
	return report.Bool()
}

func shadowInputValidity(record decisiongate.ShadowRecord) string {
	if !record.InputKnown {
		return string(handoffcheck.InputValidUnknown)
	}
	if record.InputValid {
		return string(handoffcheck.InputValidTrue)
	}
	return string(handoffcheck.InputValidFalse)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
