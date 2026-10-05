package main

import (
	"context"
	"log/slog"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
)

type publicationLeakScreen func(context.Context, string, string, string)

// newPublicationLeakScreen returns an advisory screen only after an explicit
// shadow-mode opt-in. Enforce mode intentionally does nothing until shadow
// counts justify changing publication behavior.
func newPublicationLeakScreen(cfg *instance.Config, log *slog.Logger) publicationLeakScreen {
	if cfg == nil || cfg.DecisionGate.EffectiveMode() != decisiongate.ModeShadow ||
		!cfg.DecisionGate.PublicationLeakScreen {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	gate, err := cfg.DecisionGate.Resolve(nil, func(event decisiongate.Event) {
		log.Info("decisiongate.publication-shadow",
			"question", event.Name,
			"stateDigest", event.StateDigest,
			"verdict", string(event.Outcome.Decision),
			"humanReview", event.Outcome.Decision == decisiongate.Yes,
			"probability", event.Outcome.Probability,
			"cached", event.Outcome.Cached,
			"error", errString(event.Err))
	})
	if err != nil {
		log.Warn("decisionGate publication screen disabled", "error", err.Error())
		return nil
	}
	return func(ctx context.Context, kind, title, body string) {
		_, _ = gate.EvaluatePublication(ctx, kind, title, body)
	}
}

func publicationLeakScreenForRoot(root string, log *slog.Logger) publicationLeakScreen {
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		return nil
	}
	return newPublicationLeakScreen(cfg, log)
}
