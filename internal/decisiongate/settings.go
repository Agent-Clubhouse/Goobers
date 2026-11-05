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

// newEntraTokenSource is a seam so tests need no Azure identity.
var newEntraTokenSource = decider.NewEntraTokenSource

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Settings is the instance-level opt-in. Secrets are never stored here: the
// endpoint and key are read from the named environment variables, so this
// struct is safe to commit as an example.
type Settings struct {
	Mode       Mode   `json:"mode,omitempty" yaml:"mode,omitempty"`
	BaseURLEnv string `json:"baseURLEnv,omitempty" yaml:"baseURLEnv,omitempty"`
	KeyEnv     string `json:"keyEnv,omitempty" yaml:"keyEnv,omitempty"`
	// Auth selects the credential: "key" (default, keyEnv) or "entra" (ambient
	// Azure identity such as workload or managed identity; no key is stored).
	Auth                  string `json:"auth,omitempty" yaml:"auth,omitempty"`
	ModelEnv              string `json:"modelEnv,omitempty" yaml:"modelEnv,omitempty"`
	PublicationLeakScreen bool   `json:"publicationLeakScreen,omitempty" yaml:"publicationLeakScreen,omitempty"`
	// Fallback is what a caller does on Uncertain or exhausted retries.
	Fallback string `json:"fallback,omitempty" yaml:"fallback,omitempty"`
	// ShadowSample is the fraction (0..1) of eligible handoffs scored in shadow
	// mode. Zero means 1.
	ShadowSample float64 `json:"shadowSample,omitempty" yaml:"shadowSample,omitempty"`
	Gate         Config  `json:"gate,omitempty" yaml:"gate,omitempty"`
	// AgentTool optionally exposes a budgeted decide tool to named agent tasks.
	// Absent or with no tasks means no agent stage can call the model.
	AgentTool *AgentTool `json:"agentTool,omitempty" yaml:"agentTool,omitempty"`
}

// AgentTool grants the decide MCP tool to specific agent tasks. Denied by
// default: only tasks listed here ever see the tool.
type AgentTool struct {
	// Tasks are the task IDs granted the tool.
	Tasks []string `json:"tasks,omitempty" yaml:"tasks,omitempty"`
	// MaxCalls bounds model calls per stage invocation. Zero means 5.
	MaxCalls int `json:"maxCalls,omitempty" yaml:"maxCalls,omitempty"`
}

// Agent-tool call budget bounds.
const (
	DefaultAgentToolMaxCalls = 5
	MaxAgentToolMaxCalls     = 50
)

// Granted reports whether taskID may use the decide tool.
func (a *AgentTool) Granted(taskID string) bool {
	if a == nil || taskID == "" {
		return false
	}
	for _, t := range a.Tasks {
		if t == taskID {
			return true
		}
	}
	return false
}

// EffectiveMaxCalls applies the default budget.
func (a *AgentTool) EffectiveMaxCalls() int {
	if a == nil || a.MaxCalls == 0 {
		return DefaultAgentToolMaxCalls
	}
	return a.MaxCalls
}

func (a *AgentTool) validate() error {
	if a == nil {
		return nil
	}
	if a.MaxCalls < 0 || a.MaxCalls > MaxAgentToolMaxCalls {
		return fmt.Errorf("decisionGate.agentTool.maxCalls must be within 0..%d", MaxAgentToolMaxCalls)
	}
	for _, t := range a.Tasks {
		if t == "" || t == "*" {
			return errors.New("decisionGate.agentTool.tasks must name specific task ids")
		}
	}
	return nil
}

// Credential kinds for Settings.Auth.
const (
	AuthKey   = "key"
	AuthEntra = "entra"
)

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
	switch s.Auth {
	case "", AuthKey, AuthEntra:
	default:
		return fmt.Errorf("decisionGate.auth %q must be key or entra", s.Auth)
	}
	fields := map[string]string{"baseURLEnv": s.BaseURLEnv, "keyEnv": s.KeyEnv, "modelEnv": s.ModelEnv}
	if s.Auth == AuthEntra {
		if s.KeyEnv != "" {
			return errors.New("decisionGate.keyEnv must be empty when auth is entra")
		}
		delete(fields, "keyEnv")
	}
	for field, v := range fields {
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
	if err := s.AgentTool.validate(); err != nil {
		return err
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
	dcfg := decider.Config{
		BaseURL: strings.TrimSpace(getenv(s.BaseURLEnv)),
		Model:   strings.TrimSpace(getenv(s.ModelEnv)),
	}
	if s.Auth == AuthEntra {
		ts, err := newEntraTokenSource()
		if err != nil {
			return nil, fmt.Errorf("decisionGate: %w", err)
		}
		dcfg.TokenSource = ts
	} else {
		dcfg.APIKey = strings.TrimSpace(getenv(s.KeyEnv))
	}
	cli, err := decider.New(dcfg)
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
	if _, ok := cfg.Thresholds[PublicationLeakQuestion]; !ok {
		cfg.Thresholds[PublicationLeakQuestion] = DefaultPublicationLeakThreshold
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 5 * time.Second
	}
	return New(cli, cfg, observe)
}
