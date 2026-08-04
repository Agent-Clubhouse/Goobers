package instance

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SandboxPosture is the effective isolation posture agentic stages run under
// (#1305). Which sandbox mechanism enforcement uses is the runner's concern,
// not configuration's.
type SandboxPosture string

const (
	// SandboxDisabled is the explicit trusted-local opt-out. Agentic stages run
	// directly on the host and journal that posture for every attempt.
	SandboxDisabled SandboxPosture = "disabled"
	// SandboxEnforced requires the runner to isolate agentic stages; a runner
	// that cannot must fail the run rather than fall back to the host.
	SandboxEnforced SandboxPosture = "enforced"
)

// SandboxConfig is instance.yaml's sandbox block: the instance-wide posture a
// gaggle may strengthen (GaggleSpec.Sandbox). Absent means enforced.
type SandboxConfig struct {
	// Agentic is the posture for agentic stages: "disabled" or "enforced".
	// Empty defaults to enforced.
	Agentic string `json:"agentic,omitempty" yaml:"agentic,omitempty"`
}

// Validate fails closed on a posture value the runner would otherwise have to
// guess at mid-run.
func (s SandboxConfig) Validate() error {
	switch SandboxPosture(s.Agentic) {
	case "", SandboxDisabled, SandboxEnforced:
		return nil
	default:
		return fmt.Errorf("agentic must be %q or %q, got %q", SandboxDisabled, SandboxEnforced, s.Agentic)
	}
}

// EffectiveAgenticSandbox resolves the isolation posture one gaggle's agentic
// stages run under. Sandboxing is enforced by default. Only the operator-owned
// instance.yaml may opt out to trusted-local execution; a gaggle may strengthen
// that opt-out back to enforced but may never weaken enforcement. This matters
// because a Gaggle lives in the config directory, which the Tutor and other
// less-privileged writers can reach (SEC-021). Pure — no config load or runner
// state — so the sandbox wiring and the scheduler agree on one resolution.
func EffectiveAgenticSandbox(cfg *Config, gaggle *apiv1.Gaggle) SandboxPosture {
	instancePosture := SandboxEnforced
	if cfg != nil && cfg.Sandbox != nil && cfg.Sandbox.Agentic != "" {
		instancePosture = SandboxPosture(cfg.Sandbox.Agentic)
	}
	if instancePosture == SandboxEnforced {
		return SandboxEnforced
	}
	if gaggle != nil && gaggle.Spec.Sandbox != nil &&
		SandboxPosture(gaggle.Spec.Sandbox.Agentic) == SandboxEnforced {
		return SandboxEnforced
	}
	return SandboxDisabled
}
