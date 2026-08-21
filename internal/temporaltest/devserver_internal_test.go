package temporaltest

import (
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// TestResolveDevServerAcquisition covers the decision StartDevServer makes
// from CLIEnvVar's raw value, independent of actually spawning a dev server
// process or touching the network (#3393).
func TestResolveDevServerAcquisition(t *testing.T) {
	tests := []struct {
		name             string
		rawEnv           string
		wantExistingPath string
		wantCached       testsuite.CachedDownload
		wantModeContains string
	}{
		{
			name:             "env set to a binary path uses ExistingPath",
			rawEnv:           "/opt/temporal/temporal",
			wantExistingPath: "/opt/temporal/temporal",
			wantCached:       testsuite.CachedDownload{},
			wantModeContains: "existing CLI",
		},
		{
			name:             "env unset falls back to today's cached download",
			rawEnv:           "",
			wantExistingPath: "",
			wantCached:       testsuite.CachedDownload{Version: "default"},
			wantModeContains: "cached download",
		},
		{
			name:             "whitespace-only env is treated as unset",
			rawEnv:           "   ",
			wantExistingPath: "",
			wantCached:       testsuite.CachedDownload{Version: "default"},
			wantModeContains: "cached download",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existingPath, cached, mode := resolveDevServerAcquisition(tt.rawEnv)
			if existingPath != tt.wantExistingPath {
				t.Errorf("existingPath = %q, want %q", existingPath, tt.wantExistingPath)
			}
			if cached != tt.wantCached {
				t.Errorf("cached = %+v, want %+v", cached, tt.wantCached)
			}
			if !strings.Contains(mode, tt.wantModeContains) {
				t.Errorf("mode = %q, want it to contain %q", mode, tt.wantModeContains)
			}
		})
	}
}
