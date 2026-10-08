package main

import (
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
)

// TestPRSelectWriteCapabilityAcceptsPreAlpha3Declaration pins the runtime half
// of the v0.6.0-alpha.3 regression fix: a merge-review written before alpha.3
// declares github:pr:write on pr-select, so that credential must be used when
// it is the only one delivered.
func TestPRSelectWriteCapabilityAcceptsPreAlpha3Declaration(t *testing.T) {
	providerEnv := executor.CredentialEnvVar(string(capability.ProviderPRWrite))
	githubEnv := executor.CredentialEnvVar(string(capability.GitHubPRWrite))
	tests := []struct {
		name             string
		provider, github string
		want             capability.Capability
	}{
		{name: "provider:pr:write delivered", provider: "p", want: capability.ProviderPRWrite},
		{name: "github:pr:write delivered", github: "g", want: capability.GitHubPRWrite},
		{name: "both delivered prefers provider", provider: "p", github: "g", want: capability.ProviderPRWrite},
		{name: "neither delivered names the current capability", want: capability.ProviderPRWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(providerEnv, tt.provider)
			t.Setenv(githubEnv, tt.github)
			if got := prSelectWriteCapability(); got != tt.want {
				t.Fatalf("prSelectWriteCapability() = %q, want %q", got, tt.want)
			}
		})
	}
}
