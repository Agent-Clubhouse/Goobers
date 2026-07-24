package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSandboxPosturesByGaggle pins the composition-root posture resolution
// the per-gaggle runner wiring consumes (#1305): a gaggle's own sandbox
// override beats the instance-wide sandbox.agentic posture, and an entirely
// unconfigured instance resolves every gaggle to disabled — the opt-in
// default that keeps unconfigured behavior byte-identical.
func TestSandboxPosturesByGaggle(t *testing.T) {
	gaggle := func(name, agentic string) apiv1.Gaggle {
		g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if agentic != "" {
			g.Spec.Sandbox = &apiv1.GaggleSandbox{Agentic: agentic}
		}
		return g
	}

	t.Run("gaggle override beats instance posture", func(t *testing.T) {
		cfg := &instance.Config{Sandbox: &instance.SandboxConfig{Agentic: string(instance.SandboxEnforced)}}
		set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
			gaggle("inherits", ""),
			gaggle("opts-down", string(instance.SandboxDisabled)),
		}}
		got := sandboxPosturesByGaggle(cfg, set)
		if got["inherits"] != instance.SandboxEnforced {
			t.Fatalf("inherits = %q, want the instance-wide enforced posture", got["inherits"])
		}
		if got["opts-down"] != instance.SandboxDisabled {
			t.Fatalf("opts-down = %q, want the gaggle's disabled override to beat the instance posture", got["opts-down"])
		}
	})

	t.Run("gaggle opts up without instance posture", func(t *testing.T) {
		cfg := &instance.Config{}
		set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
			gaggle("opts-up", string(instance.SandboxEnforced)),
			gaggle("default", ""),
		}}
		got := sandboxPosturesByGaggle(cfg, set)
		if got["opts-up"] != instance.SandboxEnforced {
			t.Fatalf("opts-up = %q, want enforced from the gaggle override alone", got["opts-up"])
		}
		if got["default"] != instance.SandboxDisabled {
			t.Fatalf("default = %q, want disabled when nothing is configured", got["default"])
		}
	})
}
