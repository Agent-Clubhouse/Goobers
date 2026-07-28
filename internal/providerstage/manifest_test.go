package providerstage

import (
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
)

func TestRequiredCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		want    []capability.Capability
	}{
		{
			name:    "always required and optional use",
			command: "backlog-query",
			args:    []string{"--claim"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "conditional capability absent",
			command: "telemetry-query",
			want:    []capability.Capability{capability.TelemetryRead},
		},
		{
			name:    "conditional capability split flag",
			command: "telemetry-query",
			args:    []string{"--format", "tutor-live-verification"},
			want:    []capability.Capability{capability.TelemetryRead, capability.GitHubPRWrite},
		},
		{
			name:    "conditional capability equals flag",
			command: "telemetry-query",
			args:    []string{"--format=tutor-live-verification"},
			want:    []capability.Capability{capability.TelemetryRead, capability.GitHubPRWrite},
		},
		{
			name:    "conditional capability single dash split flag",
			command: "telemetry-query",
			args:    []string{"-format", "tutor-live-verification"},
			want:    []capability.Capability{capability.TelemetryRead, capability.GitHubPRWrite},
		},
		{
			name:    "conditional capability single dash equals flag",
			command: "telemetry-query",
			args:    []string{"-format=tutor-live-verification"},
			want:    []capability.Capability{capability.TelemetryRead, capability.GitHubPRWrite},
		},
		{
			name:    "unconditional capability without flag",
			command: "reconcile-branches",
			want:    []capability.Capability{capability.GitHubBranchDelete},
		},
		{
			name:    "unconditional capability with flag",
			command: "reconcile-branches",
			args:    []string{"--delete"},
			want:    []capability.Capability{capability.GitHubBranchDelete},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uses := RequiredCapabilities(test.command, test.args)
			got := make([]capability.Capability, 0, len(uses))
			for _, use := range uses {
				got = append(got, use.Capability)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("RequiredCapabilities(%q, %q) = %q, want %q", test.command, test.args, got, test.want)
			}
		})
	}
}

func TestManifestCapabilityUsesAreActionable(t *testing.T) {
	for _, name := range Names() {
		entry, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) did not return a listed command", name)
		}
		seen := map[capability.Capability]bool{}
		for _, use := range entry.Capabilities {
			if !capability.StageDeclarable(string(use.Capability)) {
				t.Errorf("%s uses non-stage capability %q", name, use.Capability)
			}
			if seen[use.Capability] {
				t.Errorf("%s lists capability %q more than once", name, use.Capability)
			}
			seen[use.Capability] = true
			if strings.TrimSpace(use.Consequence) == "" {
				t.Errorf("%s capability %q has no runtime consequence", name, use.Capability)
			}
		}
	}
}

func TestResultFile(t *testing.T) {
	if got, ok := ResultFile("merge-queue-poll"); !ok || got != "queue-result.json" {
		t.Fatalf("ResultFile(merge-queue-poll) = %q, %v, want queue-result.json, true", got, ok)
	}
	if got, ok := ResultFile("push-branch"); ok || got != "" {
		t.Fatalf("ResultFile(push-branch) = %q, %v, want empty, false", got, ok)
	}
}
