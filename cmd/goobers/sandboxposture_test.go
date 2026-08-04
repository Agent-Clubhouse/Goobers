package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// TestSandboxPosturesByGaggle pins the composition-root posture resolution
// the per-gaggle runner wiring consumes (#1305): the most-restrictive of the
// instance and gaggle posture wins — a gaggle may strengthen but never weaken
// an operator-enforced instance — and an entirely unconfigured instance
// resolves every gaggle to enforced.
func TestSandboxPosturesByGaggle(t *testing.T) {
	gaggle := func(name, agentic string) apiv1.Gaggle {
		g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if agentic != "" {
			g.Spec.Sandbox = &apiv1.GaggleSandbox{Agentic: agentic}
		}
		return g
	}

	t.Run("gaggle cannot weaken instance-enforced posture", func(t *testing.T) {
		cfg := &instance.Config{Sandbox: &instance.SandboxConfig{Agentic: string(instance.SandboxEnforced)}}
		set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
			gaggle("inherits", ""),
			gaggle("opts-down", string(instance.SandboxDisabled)),
		}}
		got := sandboxPosturesByGaggle(cfg, set)
		if got["inherits"] != instance.SandboxEnforced {
			t.Fatalf("inherits = %q, want the instance-wide enforced posture", got["inherits"])
		}
		if got["opts-down"] != instance.SandboxEnforced {
			t.Fatalf("opts-down = %q, want the operator-enforced posture to hold against a gaggle downgrade", got["opts-down"])
		}
	})

	t.Run("gaggle opts up without instance posture", func(t *testing.T) {
		cfg := &instance.Config{}
		set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
			gaggle("opts-up", string(instance.SandboxEnforced)),
			gaggle("default", ""),
			gaggle("opts-down", string(instance.SandboxDisabled)),
		}}
		got := sandboxPosturesByGaggle(cfg, set)
		if got["opts-up"] != instance.SandboxEnforced {
			t.Fatalf("opts-up = %q, want enforced from the gaggle override alone", got["opts-up"])
		}
		if got["default"] != instance.SandboxEnforced {
			t.Fatalf("default = %q, want enforced when nothing is configured", got["default"])
		}
		if got["opts-down"] != instance.SandboxEnforced {
			t.Fatalf("opts-down = %q, want a gaggle unable to weaken default enforcement", got["opts-down"])
		}
	})

	t.Run("operator trusted-local opt-out", func(t *testing.T) {
		cfg := &instance.Config{Sandbox: &instance.SandboxConfig{Agentic: string(instance.SandboxDisabled)}}
		set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
			gaggle("inherits", ""),
			gaggle("enforces", string(instance.SandboxEnforced)),
		}}
		got := sandboxPosturesByGaggle(cfg, set)
		if got["inherits"] != instance.SandboxDisabled {
			t.Fatalf("inherits = %q, want explicit operator opt-out", got["inherits"])
		}
		if got["enforces"] != instance.SandboxEnforced {
			t.Fatalf("enforces = %q, want gaggle strengthening", got["enforces"])
		}
	})
}
