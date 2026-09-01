package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestGuidedDiscoveryUsesGoobersGitHubTokenForGitHubCLI(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "guided-test-token")

	originalCommand := guidedDiscoveryCommand
	t.Cleanup(func() {
		guidedDiscoveryCommand = originalCommand
	})
	guidedDiscoveryCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "env")
	}

	output, err := runGuidedDiscovery(context.Background(), "gh", "api", "user")
	if err != nil {
		t.Fatalf("runGuidedDiscovery() error = %v", err)
	}
	if !strings.Contains(output, "GH_TOKEN=guided-test-token\n") {
		t.Fatalf("GitHub CLI environment = %q, want GOOBERS_GITHUB_TOKEN mapped to GH_TOKEN", output)
	}
}
