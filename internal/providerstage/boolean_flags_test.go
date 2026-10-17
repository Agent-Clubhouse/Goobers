package providerstage

import (
	"testing"

	"github.com/goobers/goobers/internal/capability"
)

func TestBooleanFlagsAfterValues(t *testing.T) {
	// No shipped command currently combines a value-taking flag with a
	// boolean manifest condition. Model that combination without inventing
	// a --label flag on the real backlog-query command.
	table := map[string]Command{
		"query": {
			valueFlags:         []string{"label", "limit"},
			claimMutationFlags: []string{"claim", "release"},
			Capabilities: []CapabilityUse{
				requiredWhenAnyFlag(capability.GitHubIssuesWrite, []string{"claim", "release"}, "mutation"),
				requiredUnlessAnyFlag(capability.GitHubIssuesRead, []string{"claim", "release"}, "read only"),
			},
		},
	}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no flags"},
		{name: "bare flag", args: []string{"--claim"}, want: true},
		{name: "after spaced value", args: []string{"--label", "x", "--claim"}, want: true},
		{name: "single dash flags", args: []string{"-label", "x", "-claim"}, want: true},
		{name: "after equals value", args: []string{"--label=x", "--claim"}, want: true},
		{name: "multiple spaced values", args: []string{"--label", "x", "--limit", "3", "--claim"}, want: true},
		{name: "negative value", args: []string{"--limit", "-1", "--claim"}, want: true},
		{name: "empty value", args: []string{"--label", "", "--claim"}, want: true},
		{name: "dash value", args: []string{"--label", "-", "--claim"}, want: true},
		{name: "separator as value", args: []string{"--label", "--", "--claim"}, want: true},
		{name: "boolean name as value", args: []string{"--label", "--claim"}},
		{name: "boolean assignment as value", args: []string{"--label", "--claim=true"}},
		{name: "boolean name in equals value", args: []string{"--label=--claim"}},
		{name: "missing value", args: []string{"--label"}},
		{name: "disabled flag after value", args: []string{"--label", "x", "--claim=false"}},
		{name: "last value enables", args: []string{"--claim=false", "--label", "x", "--claim"}, want: true},
		{name: "last value disables", args: []string{"--claim", "--label", "x", "--claim=false"}},
		{name: "another flag enables", args: []string{"--claim=false", "--label", "x", "--release"}, want: true},
		{name: "value does not overwrite flag", args: []string{"--claim", "--label", "--claim=false"}, want: true},
		{name: "separator after value", args: []string{"--label", "x", "--", "--claim"}},
		{name: "positional first", args: []string{"path", "--claim"}},
		{name: "positional after value", args: []string{"--label", "x", "path", "--claim"}},
		{name: "positional after equals value", args: []string{"--label=x", "path", "--claim"}},
		{name: "dash positional", args: []string{"-", "--claim"}},
		{name: "unrelated boolean then positional", args: []string{"--debug", "path", "--claim"}},
		{name: "boolean spaced false is positional", args: []string{"--claim=false", "false", "--claim"}},
		{name: "enabled before positional", args: []string{"--claim", "path", "--claim=false"}, want: true},
	}
	for _, version := range shippedDSLVersions {
		for _, tt := range tests {
			t.Run(version+"/"+tt.name, func(t *testing.T) {
				if got := mutatesClaimLedgerIn(table, version, "query", tt.args); got != tt.want {
					t.Errorf("MutatesClaimLedger(%q) = %t, want %t", tt.args, got, tt.want)
				}
				wantCap := capability.GitHubIssuesRead
				if tt.want {
					wantCap = capability.GitHubIssuesWrite
				}
				uses := requiredCapabilitiesIn(table, version, "query", tt.args)
				if len(uses) != 1 || uses[0].Capability != wantCap {
					t.Errorf("RequiredCapabilities(%q) = %v, want %s", tt.args, uses, wantCap)
				}
			})
		}
	}
}
