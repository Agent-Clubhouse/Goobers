package localscheduler

import "testing"

func TestIsWorkItemClaimKey(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"536", true},
		{"ENG-12", true},
		{"", false},
		{"pr/536", false},
		{"advisory-pr/github/owner/repo/security/536", false},
		{"decomposition-target:3344", false},
		{"merge-lock/owner/repo", false},
	} {
		if got := IsWorkItemClaimKey(tc.id); got != tc.want {
			t.Errorf("IsWorkItemClaimKey(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
