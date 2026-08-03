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
			name:    "claiming backlog query",
			command: "backlog-query",
			args:    []string{"--claim"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "legacy backlog query",
			command: "backlog-query",
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "read-only backlog query",
			command: "backlog-query",
			args:    []string{"--read-only"},
			want:    []capability.Capability{capability.GitHubIssuesRead},
		},
		{
			name:    "single dash read-only backlog query",
			command: "backlog-query",
			args:    []string{"-read-only"},
			want:    []capability.Capability{capability.GitHubIssuesRead},
		},
		{
			name:    "bare read-only positional argument",
			command: "backlog-query",
			args:    []string{"read-only"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "read-only flag after positional argument",
			command: "backlog-query",
			args:    []string{"path", "--read-only"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "read-only flag after flag terminator",
			command: "backlog-query",
			args:    []string{"--", "--read-only"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "explicitly disabled read-only backlog query",
			command: "backlog-query",
			args:    []string{"--read-only=false"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "last read-only value disables",
			command: "backlog-query",
			args:    []string{"--read-only", "--read-only=false"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "last read-only value enables",
			command: "backlog-query",
			args:    []string{"--read-only=false", "--read-only"},
			want:    []capability.Capability{capability.GitHubIssuesRead},
		},
		{
			name:    "explicitly disabled claim backlog query",
			command: "backlog-query",
			args:    []string{"--claim=false"},
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

	for _, value := range []string{"0", "f", "F", "FALSE", "false", "False"} {
		t.Run("read-only false spelling "+value, func(t *testing.T) {
			uses := RequiredCapabilities("backlog-query", []string{"--read-only=" + value})
			got := make([]capability.Capability, 0, len(uses))
			for _, use := range uses {
				got = append(got, use.Capability)
			}
			want := []capability.Capability{capability.GitHubIssuesWrite}
			if !slices.Equal(got, want) {
				t.Fatalf("RequiredCapabilities(%q, %q) = %q, want %q", "backlog-query", "--read-only="+value, got, want)
			}
		})
	}
}

func TestManifestCapabilityUsesAreActionable(t *testing.T) {
	for name, entry := range commands {
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
