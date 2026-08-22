package providerstage

import (
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
)

// shippedDSLVersions are the DSL versions with a live interpreter today
// (internal/workflow v_current/v_next). Behavior tests run against each
// shipped view: with the table all-baseline, every view must agree.
var shippedDSLVersions = []string{"1.4", "2.0"}

func TestRequiredCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		want    []capability.Capability
	}{
		{
			name:    "provider-neutral verdict application",
			command: "apply-verdict",
			want:    []capability.Capability{capability.ProviderPRWrite, capability.GitHubPRReview},
		},
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
			name:    "provider-neutral pull request open",
			command: "open-pr",
			want:    []capability.Capability{capability.ProviderPRWrite},
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
		{
			name:    "publish decomposition batch",
			command: "publish-batch",
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "backlog health snapshot",
			command: "backlog-health",
			want:    []capability.Capability{capability.GitHubIssuesRead},
		},
		{
			name:    "backlog health feedback",
			command: "backlog-health",
			args:    []string{"--feedback"},
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
		{
			name:    "finding response validation",
			command: "respond-to-findings",
			args:    []string{"--check"},
		},
		{
			name:    "finding response publication",
			command: "respond-to-findings",
			want:    []capability.Capability{capability.GitHubIssuesWrite},
		},
	}

	for _, version := range shippedDSLVersions {
		view := ForVersion(version)
		for _, test := range tests {
			t.Run("DSL "+version+" "+test.name, func(t *testing.T) {
				uses := view.RequiredCapabilities(test.command, test.args)
				got := make([]capability.Capability, 0, len(uses))
				for _, use := range uses {
					got = append(got, use.Capability)
				}
				if !slices.Equal(got, test.want) {
					t.Fatalf("ForVersion(%q).RequiredCapabilities(%q, %q) = %q, want %q", version, test.command, test.args, got, test.want)
				}
			})
		}

		for _, value := range []string{"0", "f", "F", "FALSE", "false", "False"} {
			t.Run("DSL "+version+" read-only false spelling "+value, func(t *testing.T) {
				uses := view.RequiredCapabilities("backlog-query", []string{"--read-only=" + value})
				got := make([]capability.Capability, 0, len(uses))
				for _, use := range uses {
					got = append(got, use.Capability)
				}
				want := []capability.Capability{capability.GitHubIssuesWrite}
				if !slices.Equal(got, want) {
					t.Fatalf("ForVersion(%q).RequiredCapabilities(%q, %q) = %q, want %q", version, "backlog-query", "--read-only="+value, got, want)
				}
			})
		}
	}
}

func TestMutatesClaimLedger(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		want    bool
	}{
		{name: "backlog claim", command: "backlog-query", args: []string{"--claim"}, want: true},
		{name: "backlog reconcile", command: "backlog-query", args: []string{"--reconcile"}, want: true},
		{name: "backlog release", command: "backlog-query", args: []string{"--release=true"}, want: true},
		{name: "disabled backlog mutation", command: "backlog-query", args: []string{"--claim=false"}},
		{name: "read-only backlog query", command: "backlog-query", args: []string{"--read-only"}},
		{name: "flags terminated", command: "backlog-query", args: []string{"--", "--claim"}},
		{name: "PR selection", command: "pr-select", want: true},
		{name: "PR context", command: "gather-pr-context", want: true},
		{name: "behind PR update", command: "update-behind-pr", want: true},
		{name: "PR release", command: "pr-claim", args: []string{"--release"}, want: true},
		{name: "issue close-out", command: "issue-close-out", want: true},
		{name: "decomposition source", command: "select-source", want: true},
		{name: "unrelated command", command: "open-pr"},
	}
	for _, version := range shippedDSLVersions {
		view := ForVersion(version)
		for _, test := range tests {
			t.Run("DSL "+version+" "+test.name, func(t *testing.T) {
				if got := view.MutatesClaimLedger(test.command, test.args); got != test.want {
					t.Fatalf("ForVersion(%q).MutatesClaimLedger(%q, %q) = %t, want %t", version, test.command, test.args, got, test.want)
				}
			})
		}
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
	if got, ok := ResultFile("publish-batch"); !ok || got != "published-batch.json" {
		t.Fatalf("ResultFile(publish-batch) = %q, %v, want published-batch.json, true", got, ok)
	}
}
