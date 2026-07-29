package instance

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestEffectiveSelfIdentity(t *testing.T) {
	cfg := &Config{SelfIdentity: "instance-bot"}

	tests := []struct {
		name   string
		cfg    *Config
		gaggle *apiv1.Gaggle
		want   string
	}{
		{name: "gaggle override", cfg: cfg, gaggle: &apiv1.Gaggle{Spec: apiv1.GaggleSpec{SelfIdentity: "gaggle-bot"}}, want: "gaggle-bot"},
		{name: "instance default", cfg: cfg, gaggle: &apiv1.Gaggle{}, want: "instance-bot"},
		{name: "absent", cfg: &Config{}, gaggle: &apiv1.Gaggle{}},
		{name: "nil config", gaggle: &apiv1.Gaggle{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveSelfIdentity(tt.cfg, tt.gaggle); got != tt.want {
				t.Fatalf("EffectiveSelfIdentity() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadConfigSelfIdentity(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
selfIdentity: instance-bot
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SelfIdentity != "instance-bot" {
		t.Fatalf("selfIdentity = %q, want instance-bot", cfg.SelfIdentity)
	}
}
