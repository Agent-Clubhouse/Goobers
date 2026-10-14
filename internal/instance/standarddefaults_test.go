package instance

import "testing"

// TestStandardInitDefaults pins the defaults `goobers init
// --template=standard` and the provider compile-matrix gate both seed from.
func TestStandardInitDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, provider, kind string
		want                 GuidedOptions
	}{
		{
			name: "github", provider: "github",
			want: GuidedOptions{
				GaggleName: "example", RepoProvider: "github", RepoOwner: "your-org", RepoName: "your-repo",
				RepoTokenEnv: "GOOBERS_GITHUB_TOKEN", WorkTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
				PullRequestTokenEnv: "GOOBERS_GITHUB_PR_TOKEN", RepoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
			},
		},
		{
			// azure-cli (the seeded default for an empty kind) reads no variable.
			name: "ado default auth", provider: "ado",
			want: GuidedOptions{
				GaggleName: "example", RepoProvider: "ado", RepoOwner: "your-org", RepoProject: "your-project", RepoName: "your-repo",
			},
		},
		{
			name: "ado managed identity", provider: "ado", kind: "managed-identity",
			want: GuidedOptions{
				GaggleName: "example", RepoProvider: "ado", RepoOwner: "your-org", RepoProject: "your-project", RepoName: "your-repo",
				RepoAuthKind: "managed-identity",
			},
		},
		{
			name: "ado pat", provider: "ado", kind: ADOAuthPAT,
			want: GuidedOptions{
				GaggleName: "example", RepoProvider: "ado", RepoOwner: "your-org", RepoProject: "your-project", RepoName: "your-repo",
				RepoAuthKind: ADOAuthPAT, RepoTokenEnv: "GOOBERS_ADO_TOKEN",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StandardInitDefaults(tc.provider, tc.kind)
			if got.GaggleName != tc.want.GaggleName || got.RepoProvider != tc.want.RepoProvider ||
				got.RepoOwner != tc.want.RepoOwner || got.RepoProject != tc.want.RepoProject || got.RepoName != tc.want.RepoName ||
				got.RepoAuthKind != tc.want.RepoAuthKind || got.RepoTokenEnv != tc.want.RepoTokenEnv ||
				got.WorkTrackingTokenEnv != tc.want.WorkTrackingTokenEnv || got.PullRequestTokenEnv != tc.want.PullRequestTokenEnv ||
				got.RepoPushTokenEnv != tc.want.RepoPushTokenEnv {
				t.Fatalf("StandardInitDefaults(%q, %q) = %+v, want %+v", tc.provider, tc.kind, got, tc.want)
			}
		})
	}
}
