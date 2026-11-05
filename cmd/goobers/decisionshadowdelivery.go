package main

import (
	"os"
	"strings"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/httpapi"
)

// decisionShadowDelivery hands an agentic stage pod the provider settings for
// the opt-in decisionGate shadow observer. It rides the authenticated
// credential plane response, never the pod spec, kit or journal; the key is
// registered with the daemon scrubbers before the response is written.
func (s *daemonCredentialService) decisionShadowDelivery(resolved stageResolution) *httpapi.DecisionShadowDelivery {
	if s.config == nil {
		return nil
	}
	settings := s.config.DecisionGate
	if resolved.profile.deterministic || resolved.profile.goober == "" ||
		settings == nil || settings.EffectiveMode() != decisiongate.ModeShadow {
		return nil
	}
	delivery := &httpapi.DecisionShadowDelivery{
		Settings: *settings,
		BaseURL:  strings.TrimSpace(os.Getenv(settings.BaseURLEnv)),
		APIKey:   strings.TrimSpace(os.Getenv(settings.KeyEnv)),
		Model:    strings.TrimSpace(os.Getenv(settings.ModelEnv)),
	}
	if s.shared != nil && delivery.APIKey != "" {
		s.shared.Register([]byte(delivery.APIKey))
	}
	return delivery
}
