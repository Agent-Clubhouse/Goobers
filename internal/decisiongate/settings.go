package decisiongate

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/decider"
)

// Mode is how an opted-in decision is used.
type Mode string

// Modes. Off is the default: nothing is sent to any endpoint.
const (
	ModeOff     Mode = "off"
	ModeShadow  Mode = "shadow"
	ModeEnforce Mode = "enforce"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Settings is the instance-level opt-in. Secrets are never stored here: the
// endpoint and key are read from the named environment variables, so this
// struct is safe to commit as an example.
type Settings struct {
	Mode       Mode   `json:"mode,omitempty" yaml:"mode,omitempty"`
	BaseURLEnv string `json:"baseURLEnv,omitempty" yaml:"baseURLEnv,omitempty"`
	KeyEnv     string `json:"keyEnv,omitempty" yaml:"keyEnv,omitempty"`
	ModelEnv   string `json:"modelEnv,omitempty" yaml:"modelEnv,omitempty"`
	// Fallback is what a caller does on Uncertain or exhausted retries.
	Fallback string `json:"fallback,omitempty" yaml:"fallback,omitempty"`
	// ShadowSample is the fraction (0..1) of eligible handoffs scored in shadow
	// mode. Zero means 1.
	ShadowSample float64 `json:"shadowSample,omitempty" yaml:"shadowSample,omitempty"`
	Gate         Config  `json:"gate,omitempty" yaml:"gate,omitempty"`
}

// Allowed fallbacks. agent keeps today's behavior.
const (
	FallbackAgent    = "agent"
	FallbackEscalate = "escalate"
	FallbackFail     = "fail"
)

// EffectiveMode treats an absent mode as off.
func (s *Settings) EffectiveMode() Mode {
	if s == nil || s.Mode == "" {
		return ModeOff
	}
	return s.Mode
}

// Validate rejects settings that could leak or mis-route. Inline secrets are
// refused by shape: every *Env field must be an environment variable NAME.
func (s *Settings) Validate() error {
	if s == nil {
		return nil
	}
	switch s.EffectiveMode() {
	case ModeOff:
		return nil
	case ModeShadow, ModeEnforce:
	default:
		return fmt.Errorf("decisionGate.mode %q must be off, shadow or enforce", s.Mode)
	}
	for field, v := range map[string]string{"baseURLEnv": s.BaseURLEnv, "keyEnv": s.KeyEnv, "modelEnv": s.ModelEnv} {
		if v == "" {
			return fmt.Errorf("decisionGate.%s is required when the gate is on", field)
		}
		if !envName.MatchString(v) {
			return fmt.Errorf("decisionGate.%s must be an environment variable name, not a value", field)
		}
	}
	switch s.Fallback {
	case FallbackAgent, FallbackEscalate, FallbackFail:
	default:
		return errors.New("decisionGate.fallback is required: agent, escalate or fail")
	}
	if s.ShadowSample < 0 || s.ShadowSample > 1 {
		return errors.New("decisionGate.shadowSample must be within 0..1")
	}
	return s.Gate.Validate()
}

// Resolve reads the endpoint settings from the environment and builds a Gate.
// It returns (nil, nil) when the mode is off.
func (s *Settings) Resolve(getenv func(string) string, observe func(Event)) (*Gate, error) {
	if s.EffectiveMode() == ModeOff {
		return nil, nil
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	cli, err := decider.New(decider.Config{
		BaseURL: strings.TrimSpace(getenv(s.BaseURLEnv)),
		APIKey:  strings.TrimSpace(getenv(s.KeyEnv)),
		Model:   strings.TrimSpace(getenv(s.ModelEnv)),
	})
	if err != nil {
		return nil, fmt.Errorf("decisionGate: %w", err)
	}
	cfg := s.Gate
	// Copy before defaulting so Resolve never mutates the caller's settings.
	cfg.Thresholds = maps.Clone(cfg.Thresholds)
	if cfg.Thresholds == nil {
		cfg.Thresholds = map[string]Threshold{}
	}
	if _, ok := cfg.Thresholds[ClaimQuestion]; !ok {
		cfg.Thresholds[ClaimQuestion] = DefaultClaimThreshold
	}
	if _, ok := cfg.Thresholds[PRDescriptionAgreementQuestion]; !ok {
		cfg.Thresholds[PRDescriptionAgreementQuestion] = DefaultPRDescriptionAgreementThreshold
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 5 * time.Second
	}
	return New(cli, cfg, observe)
}
