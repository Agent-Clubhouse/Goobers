package providers

import "testing"

func TestMutationWorkItemURLFromTypedGitHubReceipt(t *testing.T) {
	for _, test := range []struct {
		name       string
		repository string
		want       string
	}{
		{
			name:       "github dot com",
			repository: "https://api.github.com/repos/acme/app",
			want:       "https://github.com/acme/app/pull/9",
		},
		{
			name:       "github enterprise",
			repository: "https://github.example/api/v3/repos/acme/app",
			want:       "https://github.example/acme/app/pull/9",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			confirmation := &MergeConfirmation{RepositoryAPIURL: test.repository, PullID: "9"}
			if got := MutationWorkItemURL("github", "pr", "9", "", confirmation, nil, nil); got != test.want {
				t.Fatalf("MutationWorkItemURL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMutationWorkItemURLPreservesExistingEvidence(t *testing.T) {
	confirmation := &MergeConfirmation{RepositoryAPIURL: "https://api.github.com/repos/acme/app", PullID: "9"}
	const existing = "https://example.test/custom/9"
	if got := MutationWorkItemURL("github", "pr", "9", existing, confirmation, nil, nil); got != existing {
		t.Fatalf("MutationWorkItemURL = %q, want existing %q", got, existing)
	}
}
