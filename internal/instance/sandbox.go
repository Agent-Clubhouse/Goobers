package instance

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SandboxPosture is the effective isolation posture agentic stages run under
// (#1305). Only "disabled" and "enforced" exist: posture is a binary opt-in,
// not a ladder rung — which sandbox mechanism enforcement uses is the
// runner's concern, not configuration's.
type SandboxPosture string

const (
	// SandboxDisabled runs agentic stages directly on the host — the default,
	// byte-identical to behavior before the sandbox surface existed.
	SandboxDisabled SandboxPosture = "disabled"
	// SandboxEnforced requires the runner to isolate agentic stages; a runner
	// that cannot must fail the run rather than fall back to the host.
	SandboxEnforced SandboxPosture = "enforced"
)

// SandboxConfig is instance.yaml's sandbox block: the instance-wide posture a
// gaggle may override (GaggleSpec.Sandbox). Absent means disabled.
type SandboxConfig struct {
	// Agentic is the posture for agentic stages: "disabled" or "enforced".
	// Empty defaults to disabled.
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
// stages run under. It takes the MOST-RESTRICTIVE of the instance-wide posture
// and the gaggle's own posture: a gaggle may STRENGTHEN a disabled instance to
// enforced, but it may never WEAKEN an operator-enforced instance back to
// disabled. This matters because instance.yaml is the operator-controlled trust
// root while a Gaggle lives in the config directory, which the Tutor and other
// less-privileged writers can reach (SEC-021) — a gaggle file must not be able
// to silently opt a run out of an operator's mandated confinement. Absent on
// both sides means disabled (the default, byte-identical to before). Pure — no
// config load or runner state — so the sandbox wiring and the scheduler agree
// on one resolution.
func EffectiveAgenticSandbox(cfg *Config, gaggle *apiv1.Gaggle) SandboxPosture {
	instancePosture := SandboxDisabled
	if cfg != nil && cfg.Sandbox != nil && cfg.Sandbox.Agentic != "" {
		instancePosture = SandboxPosture(cfg.Sandbox.Agentic)
	}
	gagglePosture := instancePosture
	if gaggle != nil && gaggle.Spec.Sandbox != nil && gaggle.Spec.Sandbox.Agentic != "" {
		gagglePosture = SandboxPosture(gaggle.Spec.Sandbox.Agentic)
	}
	if instancePosture == SandboxEnforced || gagglePosture == SandboxEnforced {
		return SandboxEnforced
	}
	return SandboxDisabled
}
