package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGuidedDiscoveryUsesGoobersGitHubTokenForGitHubCLI(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "guided-test-token")

	cmd := &exec.Cmd{}
	configureGuidedDiscoveryCommand(cmd, "gh")
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "GH_TOKEN=guided-test-token") {
		t.Fatalf("GitHub CLI environment = %q, want GOOBERS_GITHUB_TOKEN mapped to GH_TOKEN", cmd.Env)
	}
}
