package providers

import "testing"

// TestParseADORepositoryURL pins every Azure DevOps remote/URL shape the
// binary accepts, plus the negatives that must fail closed (ADO-N35).
func TestParseADORepositoryURL(t *testing.T) {
	type want struct {
		org, project, repo string
	}
	forOrg := want{org: "contoso", project: "example-project", repo: "example-repo"}
	cases := []struct {
		name  string
		input string
		want  want
		ok    bool
	}{
		{"bare triple", "contoso/example-project/example-repo", forOrg, true},
		{"bare triple with .git", "contoso/example-project/example-repo.git", forOrg, true},
		{"bare triple with whitespace", "  contoso/example-project/example-repo  ", forOrg, true},
		{"dev.azure.com", "https://dev.azure.com/contoso/example-project/_git/example-repo", forOrg, true},
		{"dev.azure.com with .git", "https://dev.azure.com/contoso/example-project/_git/example-repo.git", forOrg, true},
		{"dev.azure.com uppercase host", "https://DEV.AZURE.COM/contoso/example-project/_git/example-repo", forOrg, true},
		{
			"dev.azure.com userinfo",
			"https://contoso@dev.azure.com/contoso/example-project/_git/example-repo",
			forOrg, true,
		},
		{
			"dev.azure.com short form (project == repo)",
			"https://dev.azure.com/contoso/_git/example-repo",
			want{org: "contoso", project: "example-repo", repo: "example-repo"}, true,
		},
		{
			"dev.azure.com percent-escaped project",
			"https://dev.azure.com/contoso/Example%20Project/_git/example-repo",
			want{org: "contoso", project: "Example Project", repo: "example-repo"}, true,
		},
		{"visualstudio.com", "https://contoso.visualstudio.com/example-project/_git/example-repo", forOrg, true},
		{
			"visualstudio.com with DefaultCollection",
			"https://contoso.visualstudio.com/DefaultCollection/example-project/_git/example-repo",
			forOrg, true,
		},
		{
			"visualstudio.com uppercase host",
			"https://CONTOSO.VISUALSTUDIO.COM/example-project/_git/example-repo",
			forOrg, true,
		},
		{
			"visualstudio.com short form (project == repo)",
			"https://contoso.visualstudio.com/_git/example-repo",
			want{org: "contoso", project: "example-repo", repo: "example-repo"}, true,
		},
		{"ssh.dev.azure.com scp-like", "git@ssh.dev.azure.com:v3/contoso/example-project/example-repo", forOrg, true},
		{
			"vs-ssh.visualstudio.com scp-like",
			"contoso@vs-ssh.visualstudio.com:v3/contoso/example-project/example-repo",
			forOrg, true,
		},
		{
			"vs-ssh.visualstudio.com uppercase host",
			"contoso@VS-SSH.VISUALSTUDIO.COM:v3/contoso/example-project/example-repo",
			forOrg, true,
		},

		// Negatives.
		{"empty", "", want{}, false},
		{"bare owner only", "acme", want{}, false},
		{"bare owner/name (two segments)", "acme/web", want{}, false},
		{"bare four segments", "acme/web/extra/more", want{}, false},
		{"github.com https", "https://github.com/acme/web", want{}, false},
		{"github.com scp-like", "git@github.com:acme/web.git", want{}, false},
		{"gitlab.com", "https://gitlab.com/acme/group/web", want{}, false},
		{"gitea host", "https://gitea.example.com/acme/web", want{}, false},
		{"double slash", "acme//web", want{}, false},
		{"self-hosted ADO Server host", "https://ado.example-corp.internal/contoso/example-project/_git/example-repo", want{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org, project, repo, ok := ParseADORepositoryURL(tc.input)
			if ok != tc.ok {
				t.Fatalf("ParseADORepositoryURL(%q) ok = %v, want %v (org=%q project=%q repo=%q)", tc.input, ok, tc.ok, org, project, repo)
			}
			if !ok {
				return
			}
			if org != tc.want.org || project != tc.want.project || repo != tc.want.repo {
				t.Errorf("ParseADORepositoryURL(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.input, org, project, repo, tc.want.org, tc.want.project, tc.want.repo)
			}
		})
	}
}
