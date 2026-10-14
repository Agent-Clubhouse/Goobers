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
			"ssh.dev.azure.com ssh:// form",
			"ssh://git@ssh.dev.azure.com/v3/contoso/example-project/example-repo",
			forOrg, true,
		},
		{
			"vs-ssh.visualstudio.com ssh:// form (org not read from host)",
			"ssh://contoso@vs-ssh.visualstudio.com/v3/contoso/example-project/example-repo",
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

// TestParseADORemoteURL pins the host-anchored normaliser the routing and
// matching callers use: it must accept Azure DevOps project/repository names
// outside connect's character class, and must never read a bare slug or local
// path as ADO.
func TestParseADORemoteURL(t *testing.T) {
	cases := []struct {
		name               string
		input              string
		org, project, repo string
		ok                 bool
	}{
		{"dev.azure.com", "https://dev.azure.com/example-org/example-project/_git/example-repo", "example-org", "example-project", "example-repo", true},
		{"non-ASCII project", "https://dev.azure.com/example-org/Caf%C3%A9/_git/example-repo", "example-org", "Café", "example-repo", true},
		{"non-ASCII repo", "https://dev.azure.com/example-org/example-project/_git/repo_%C3%BC", "example-org", "example-project", "repo_ü", true},
		{"ampersand project", "https://dev.azure.com/example-org/Team&Ops/_git/example-repo", "example-org", "Team&Ops", "example-repo", true},
		{"visualstudio.com DefaultCollection", "https://example-org.visualstudio.com/DefaultCollection/example-project/_git/example-repo", "example-org", "example-project", "example-repo", true},
		{"scp-like ssh escaped project", "git@ssh.dev.azure.com:v3/example-org/Example%20Project/example-repo", "example-org", "Example Project", "example-repo", true},
		{"ssh:// ssh.dev.azure.com", "ssh://git@ssh.dev.azure.com/v3/example-org/example-project/example-repo", "example-org", "example-project", "example-repo", true},
		{"ssh:// vs-ssh.visualstudio.com", "ssh://example-org@vs-ssh.visualstudio.com/v3/example-org/example-project/example-repo", "example-org", "example-project", "example-repo", true},

		// Negatives: never ADO without an ADO host.
		{"bare slug", "example-org/example-project/example-repo", "", "", "", false},
		{"absolute local path", "/srv/acme/web", "", "", "", false},
		{"absolute local path .git", "/srv/acme/web.git", "", "", "", false},
		{"relative local path", "mirrors/acme/web", "", "", "", false},
		{"file URL", "file:///srv/acme/web", "", "", "", false},
		{"github.com", "https://github.com/acme/web", "", "", "", false},
		{"two segments on dev.azure.com", "https://dev.azure.com/example-org", "", "", "", false},
		{"escaped slash smuggled into a segment", "https://dev.azure.com/example-org/a%2Fb/_git/example-repo", "", "", "", false},
		{"ssh four segments without v3", "ssh://git@ssh.dev.azure.com/extra/example-org/example-project/example-repo", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org, project, repo, ok := ParseADORemoteURL(tc.input)
			if ok != tc.ok || org != tc.org || project != tc.project || repo != tc.repo {
				t.Fatalf("ParseADORemoteURL(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tc.input, org, project, repo, ok, tc.org, tc.project, tc.repo, tc.ok)
			}
		})
	}
}

// TestIsADORemoteURLAgreesWithParserAnchoring pins #5960: the fail-closed
// ADO classifier recognizes every host spelling ParseADORemoteURL anchors —
// including the SSH hosts the old dev.azure.com/visualstudio.com check missed
// — and nothing that is not anchored to an Azure DevOps host.
func TestIsADORemoteURLAgreesWithParserAnchoring(t *testing.T) {
	for _, remote := range []string{
		"https://dev.azure.com/example-org/example-project/_git/example-repo",
		"https://example-org.visualstudio.com/example-project/_git/example-repo",
		"git@ssh.dev.azure.com:v3/example-org/example-project/example-repo",
		"ssh://git@ssh.dev.azure.com/v3/example-org/example-project/example-repo",
		"example-org@vs-ssh.visualstudio.com:v3/example-org/example-project/example-repo",
		"ssh://example-org@vs-ssh.visualstudio.com/v3/example-org/example-project/example-repo",
		"SSH://GIT@SSH.DEV.AZURE.COM/v3/example-org/example-project/example-repo",
		// Anchored to an ADO host even though no full identity parses: still
		// ADO, so an unroutable origin is refused rather than sent a GitHub token.
		"https://dev.azure.com/example-org",
		"ssh://git@ssh.dev.azure.com/extra/example-org/example-project/example-repo",
	} {
		if !IsADORemoteURL(remote) {
			t.Errorf("IsADORemoteURL(%q) = false, want true", remote)
		}
	}
	for _, remote := range []string{
		"https://github.com/acme/web",
		"git@github.com:acme/web.git",
		"example-org/example-project/example-repo",
		"/srv/acme/web",
		"file:///srv/acme/web",
		"https://dev.azure.com.example.invalid/example-org/example-project/_git/example-repo",
		"",
	} {
		if IsADORemoteURL(remote) {
			t.Errorf("IsADORemoteURL(%q) = true, want false", remote)
		}
	}
}

// TestParseADOPullRequestAPIURL pins the parser run --pr uses to verify an
// API-form pull request URL by GUID (#5960).
func TestParseADOPullRequestAPIURL(t *testing.T) {
	const projectID, repositoryID = "11111111-2222-3333-4444-555555555555", "66666666-7777-8888-9999-000000000000"
	for _, tc := range []struct {
		raw                      string
		org, project, repository string
		number                   int
		ok                       bool
	}{
		{"https://dev.azure.com/acme/" + projectID + "/_apis/git/repositories/" + repositoryID + "/pullRequests/42", "acme", projectID, repositoryID, 42, true},
		{"https://dev.azure.com/acme/Example%20Service/_apis/git/repositories/web/pullrequests/7", "acme", "Example Service", "web", 7, true},
		{"https://ACME.visualstudio.com/" + projectID + "/_apis/git/repositories/" + repositoryID + "/pullRequests/42", "acme", projectID, repositoryID, 42, true},
		{"https://dev.azure.com/acme/platform/_git/web/pullrequest/42", "", "", "", 0, false},
		{"https://dev.azure.com/acme/platform/_apis/git/repositories/web/pullRequests/abc", "", "", "", 0, false},
		{"https://dev.azure.com/platform/_apis/git/repositories/web/pullRequests/42", "", "", "", 0, false},
		{"https://example.invalid/acme/platform/_apis/git/repositories/web/pullRequests/42", "", "", "", 0, false},
		{"https://a.b.visualstudio.com/platform/_apis/git/repositories/web/pullRequests/42", "", "", "", 0, false},
		{"", "", "", "", 0, false},
	} {
		org, project, repository, number, ok := ParseADOPullRequestAPIURL(tc.raw)
		if org != tc.org || project != tc.project || repository != tc.repository || number != tc.number || ok != tc.ok {
			t.Errorf("ParseADOPullRequestAPIURL(%q) = (%q, %q, %q, %d, %v), want (%q, %q, %q, %d, %v)",
				tc.raw, org, project, repository, number, ok, tc.org, tc.project, tc.repository, tc.number, tc.ok)
		}
	}
	if !IsADOGUID(projectID) || !IsADOGUID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE") || IsADOGUID("web") || IsADOGUID(projectID+"0") {
		t.Fatal("IsADOGUID misclassifies")
	}
}
