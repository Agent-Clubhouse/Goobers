package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

type publicationLeakScreen func(context.Context, string, string, string)

// newPublicationLeakScreen returns an advisory screen only after an explicit
// shadow-mode opt-in. Enforce mode intentionally does nothing until shadow
// counts justify changing publication behavior.
func newPublicationLeakScreen(cfg *instance.Config, log *slog.Logger) publicationLeakScreen {
	return newPublicationLeakScreenWithGetenv(cfg, log, nil)
}

func newPublicationLeakScreenWithGetenv(cfg *instance.Config, log *slog.Logger, getenv func(string) string) publicationLeakScreen {
	if cfg == nil || cfg.DecisionGate.EffectiveMode() != decisiongate.ModeShadow ||
		!cfg.DecisionGate.PublicationLeakScreen {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	gate, err := cfg.DecisionGate.Resolve(getenv, func(event decisiongate.Event) {
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
	if podAttempt := strings.TrimSpace(os.Getenv(dispatcher.EnvPodAttempt)); podAttempt != "" {
		encoded := strings.TrimSpace(os.Getenv(dispatcher.EnvPublicationLeakScreen))
		if encoded == "" {
			return nil
		}
		var delivery dispatcher.PublicationLeakScreen
		if err := json.Unmarshal([]byte(encoded), &delivery); err != nil {
			if log == nil {
				log = slog.Default()
			}
			log.Warn("decisionGate publication screen disabled", "error", "invalid daemon delivery: "+err.Error())
			return nil
		}
		cfg := &instance.Config{DecisionGate: &delivery.Settings}
		return newPublicationLeakScreenWithGetenv(cfg, log, func(name string) string {
			switch name {
			case delivery.Settings.BaseURLEnv:
				return delivery.BaseURL
			case delivery.Settings.KeyEnv:
				return delivery.APIKey
			case delivery.Settings.ModelEnv:
				return delivery.Model
			default:
				return ""
			}
		})
	}
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		return nil
	}
	return newPublicationLeakScreen(cfg, log)
}
