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
		{"publication-lock/github/owner/repo", false},
	} {
		if got := IsWorkItemClaimKey(tc.id); got != tc.want {
			t.Errorf("IsWorkItemClaimKey(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestPublicationClaimNamespaceDoesNotCaptureMergeLocks(t *testing.T) {
	for _, id := range []string{"publication-lock/github/owner/repo", "merge-lock/publication/github/owner/repo"} {
		if !IsPublicationClaimKey(id) {
			t.Errorf("publication custody not classified: %s", id)
		}
	}
	for _, id := range []string{"merge-lock/publication/repo", "merge-lock/owner/repo", "42"} {
		if IsPublicationClaimKey(id) {
			t.Errorf("ordinary claim classified as publication custody: %s", id)
		}
	}
}
