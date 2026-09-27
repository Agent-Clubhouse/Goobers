package providers

import "testing"

// TestWorkItemAssigneeMatches covers AssigneeMatches's case-insensitive,
// either-form comparison (#5556): a configured identity value must match
// either the item's primary Assignee (e.g. an Azure DevOps display name, or
// a GitHub/Gitea login) or any AssigneeAliases entry (e.g. an Azure DevOps
// uniqueName account identifier), case-insensitively, without breaking the
// existing unassigned-only ("") mode.
func TestWorkItemAssigneeMatches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		item       WorkItem
		configured string
		want       bool
	}{
		{
			name:       "matches by display name",
			item:       WorkItem{Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
			configured: "Alex Example",
			want:       true,
		},
		{
			name:       "matches by uniqueName alias",
			item:       WorkItem{Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
			configured: "alex@example.com",
			want:       true,
		},
		{
			name:       "matches by uniqueName alias with different case",
			item:       WorkItem{Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
			configured: "ALEX@Example.com",
			want:       true,
		},
		{
			name:       "matches by display name with different case",
			item:       WorkItem{Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
			configured: "alex example",
			want:       true,
		},
		{
			name:       "no alias, GitHub-style login match",
			item:       WorkItem{Assignee: "octocat"},
			configured: "octocat",
			want:       true,
		},
		{
			name:       "no alias, unrelated configured value",
			item:       WorkItem{Assignee: "octocat"},
			configured: "not-octocat",
			want:       false,
		},
		{
			name:       "unrelated identity does not match despite an alias present",
			item:       WorkItem{Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
			configured: "someone-else@example.com",
			want:       false,
		},
		{
			name:       "empty configured value matches only an unassigned item",
			item:       WorkItem{Assignee: ""},
			configured: "",
			want:       true,
		},
		{
			name:       "empty configured value does not match an assigned item",
			item:       WorkItem{Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
			configured: "",
			want:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.AssigneeMatches(tc.configured); got != tc.want {
				t.Errorf("AssigneeMatches(%q) = %v, want %v (item=%#v)", tc.configured, got, tc.want, tc.item)
			}
		})
	}
}
