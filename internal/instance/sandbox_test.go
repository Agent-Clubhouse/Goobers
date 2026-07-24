package instance

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSandboxConfigValidate(t *testing.T) {
	for _, value := range []string{"", "disabled", "enforced"} {
		if err := (SandboxConfig{Agentic: value}).Validate(); err != nil {
			t.Errorf("SandboxConfig{Agentic: %q}.Validate() = %v, want nil", value, err)
		}
	}
	err := (SandboxConfig{Agentic: "paranoid"}).Validate()
	if err == nil || !strings.Contains(err.Error(), `"paranoid"`) {
		t.Fatalf("invalid posture error = %v, want value named", err)
	}
}

func TestLoadConfigSandbox(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
sandbox:
  agentic: enforced
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sandbox == nil || cfg.Sandbox.Agentic != string(SandboxEnforced) {
		t.Fatalf("sandbox = %+v, want agentic enforced", cfg.Sandbox)
	}

	bad := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
sandbox:
  agentic: yes-please
`)
	if _, err := LoadConfig(bad); err == nil || !strings.Contains(err.Error(), "sandbox:") {
		t.Fatalf("invalid posture must fail at load, got %v", err)
	}
}

// TestEffectiveAgenticSandbox pins the resolution order the sandbox wiring
// consumes: gaggle override, else instance posture, else disabled (#1305 —
// DEFAULT OFF; an unconfigured instance must resolve disabled everywhere).
func TestEffectiveAgenticSandbox(t *testing.T) {
	gaggleWith := func(agentic string) *apiv1.Gaggle {
		g := &apiv1.Gaggle{}
		if agentic != "" {
			g.Spec.Sandbox = &apiv1.GaggleSandbox{Agentic: agentic}
		}
		return g
	}
	cases := []struct {
		name   string
		cfg    *Config
		gaggle *apiv1.Gaggle
		want   SandboxPosture
	}{
		{name: "nothing configured", want: SandboxDisabled},
		{name: "nil gaggle, nil config", cfg: nil, gaggle: nil, want: SandboxDisabled},
		{name: "instance disabled", cfg: &Config{Sandbox: &SandboxConfig{Agentic: "disabled"}}, want: SandboxDisabled},
		{name: "instance enforced", cfg: &Config{Sandbox: &SandboxConfig{Agentic: "enforced"}}, want: SandboxEnforced},
		{
			name:   "gaggle inherits instance",
			cfg:    &Config{Sandbox: &SandboxConfig{Agentic: "enforced"}},
			gaggle: gaggleWith(""),
			want:   SandboxEnforced,
		},
		{
			name:   "gaggle overrides instance off",
			cfg:    &Config{Sandbox: &SandboxConfig{Agentic: "enforced"}},
			gaggle: gaggleWith("disabled"),
			want:   SandboxDisabled,
		},
		{
			name:   "gaggle overrides instance on",
			cfg:    &Config{Sandbox: &SandboxConfig{Agentic: "disabled"}},
			gaggle: gaggleWith("enforced"),
			want:   SandboxEnforced,
		},
		{
			name:   "gaggle enforced with no instance block",
			gaggle: gaggleWith("enforced"),
			want:   SandboxEnforced,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveAgenticSandbox(tc.cfg, tc.gaggle); got != tc.want {
				t.Fatalf("EffectiveAgenticSandbox = %q, want %q", got, tc.want)
			}
		})
	}
}
