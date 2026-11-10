package runtimewiring

import (
	"log/slog"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
)

type stallWindowAdapter struct{ observer *decisiongate.StallObserver }

func stallStreamKey(env apiv1.InvocationEnvelope, stream string) string {
	return env.TaskID + "|" + stream
}

func (a stallWindowAdapter) ObserveWindow(env apiv1.InvocationEnvelope, stream string, window []byte) {
	a.observer.ObserveWindow(stallStreamKey(env, stream), string(window))
}

func (a stallWindowAdapter) FinishStage(env apiv1.InvocationEnvelope, budgetHit bool) {
	for _, key := range a.observer.StreamsWithPrefix(env.TaskID + "|") {
		a.observer.FinishStream(key, budgetHit)
	}
}

// DecisionStallObserver returns the advisory transcript-window observer when
// the instance opted in to decisionGate shadow mode with stallDetection, else
// nil. An unusable gate degrades to nil: shadow mode must never stop a run.
func DecisionStallObserver(cfg *instance.Config, log *slog.Logger) harness.TranscriptWindowObserver {
	if cfg == nil {
		return nil
	}
	settings := cfg.DecisionGate
	if settings == nil || settings.EffectiveMode() != decisiongate.ModeShadow || !settings.StallDetection {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	gate, err := settings.Resolve(nil, nil)
	if err != nil {
		log.Warn("decisionGate stall detection disabled", "error", err.Error())
		return nil
	}
	return stallWindowAdapter{observer: newStallObserver(gate, settings.ShadowSample, log)}
}

func newStallObserver(gate *decisiongate.Gate, sample float64, log *slog.Logger) *decisiongate.StallObserver {
	return decisiongate.NewStallObserver(gate, sample, 2,
		func(r decisiongate.StallRecord) {
			errText := ""
			if r.Err != nil {
				errText = r.Err.Error()
			}
			log.Info("decisiongate.stall-shadow",
				"stream", r.Stream, "window", r.Window, "verdict", string(r.Verdict), "basis", string(r.Basis),
				"loopCycles", r.Facts.LoopCycles, "fileChanged", r.Facts.FileChanged,
				"probability", r.Probability, "error", errText)
		},
		func(o decisiongate.StallOutcome) {
			log.Info("decisiongate.stall-outcome",
				"stream", o.Stream, "windows", o.Windows, "flaggedWindow", o.FlaggedWindow, "budgetHit", o.BudgetHit)
		})
}
