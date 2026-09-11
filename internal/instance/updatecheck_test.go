package instance

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/selfupdate"
)

// An absent block must behave exactly like the defaults, so an existing
// instance.yaml gains the check without being edited.
func TestUpdateCheckDefaults(t *testing.T) {
	var cfg *Config
	settings := cfg.UpdateCheckSettings()
	if !settings.EnabledEffective() {
		t.Error("EnabledEffective() = false for an absent block, want true (opt-out)")
	}
	if settings.ChannelEffective() != selfupdate.ChannelStable {
		t.Errorf("ChannelEffective() = %q, want %q", settings.ChannelEffective(), selfupdate.ChannelStable)
	}
	interval, err := settings.IntervalDuration()
	if err != nil {
		t.Fatalf("IntervalDuration() error = %v", err)
	}
	if interval != DefaultUpdateCheckInterval {
		t.Errorf("IntervalDuration() = %s, want %s", interval, DefaultUpdateCheckInterval)
	}
}

func TestUpdateCheckExplicitlyDisabled(t *testing.T) {
	disabled := false
	cfg := &Config{UpdateCheck: &UpdateCheckConfig{Enabled: &disabled}}
	if cfg.UpdateCheckSettings().EnabledEffective() {
		t.Error("EnabledEffective() = true for enabled: false")
	}
}

func TestUpdateCheckValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  UpdateCheckConfig
		wantErr string
	}{
		{name: "defaults"},
		{name: "stable", config: UpdateCheckConfig{Channel: selfupdate.ChannelStable}},
		{name: "prerelease", config: UpdateCheckConfig{Channel: selfupdate.ChannelPrerelease}},
		{name: "interval", config: UpdateCheckConfig{Interval: "6h"}},
		{name: "mirror", config: UpdateCheckConfig{Owner: "acme", Repository: "app"}},
		{name: "unknown channel", config: UpdateCheckConfig{Channel: "beta"}, wantErr: "updateCheck.channel"},
		{name: "unparsable interval", config: UpdateCheckConfig{Interval: "soon"}, wantErr: "must be a duration"},
		{name: "zero interval", config: UpdateCheckConfig{Interval: "0s"}, wantErr: "must be positive"},
		{name: "negative interval", config: UpdateCheckConfig{Interval: "-1h"}, wantErr: "must be positive"},
		{name: "owner without repository", config: UpdateCheckConfig{Owner: "acme"}, wantErr: "must be set together"},
		{name: "repository without owner", config: UpdateCheckConfig{Repository: "app"}, wantErr: "must be set together"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestUpdateCheckIntervalDuration(t *testing.T) {
	interval, err := UpdateCheckConfig{Interval: "90m"}.IntervalDuration()
	if err != nil {
		t.Fatalf("IntervalDuration() error = %v", err)
	}
	if interval != 90*time.Minute {
		t.Errorf("IntervalDuration() = %s, want 90m", interval)
	}
}

// An invalid channel must fail at config load rather than at the first tick,
// so the operator learns about it when they edit the file.
func TestUpdateCheckRejectedByConfigValidation(t *testing.T) {
	raw := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: app
updateCheck:
  channel: beta
`
	path := writeInstanceYAML(t, raw)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "updateCheck.channel") {
		t.Fatalf("LoadConfig() error = %v, want an updateCheck.channel rejection", err)
	}
}

func TestUpdateCheckAcceptedByConfigValidation(t *testing.T) {
	raw := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: app
    token:
      env: GITHUB_TOKEN
updateCheck:
  enabled: true
  channel: prerelease
  interval: 6h
`
	path := writeInstanceYAML(t, raw)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	settings := cfg.UpdateCheckSettings()
	if settings.ChannelEffective() != selfupdate.ChannelPrerelease {
		t.Errorf("ChannelEffective() = %q, want prerelease", settings.ChannelEffective())
	}
	interval, err := settings.IntervalDuration()
	if err != nil || interval != 6*time.Hour {
		t.Errorf("IntervalDuration() = %s, %v, want 6h", interval, err)
	}
}
