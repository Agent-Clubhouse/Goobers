package temporaltest

import (
	"regexp"
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
			name:             "env unset selects the pinned cached download",
			rawEnv:           "",
			wantExistingPath: "",
			wantCached:       testsuite.CachedDownload{Version: CLIVersion},
			wantModeContains: "cached download (version=" + CLIVersion + ")",
		},
		{
			name:             "whitespace-only env is treated as unset",
			rawEnv:           "   ",
			wantExistingPath: "",
			wantCached:       testsuite.CachedDownload{Version: CLIVersion},
			wantModeContains: "cached download (version=" + CLIVersion + ")",
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

func TestDevServerCLIVersionIsPinned(t *testing.T) {
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(CLIVersion) {
		t.Fatalf("CLIVersion = %q, want an explicit release, not a floating download alias", CLIVersion)
	}
}
