package instance

import (
	"strings"
	"testing"
)

func TestExternalGitHubAppTokenConfigRoundTrip(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: EXTERNAL_INSTALLATION_TOKEN
    auth:
      kind: github-app-token
      slug: qualification
  - provider: github
    owner: acme
    name: other
    token:
      env: PAT
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos[0].GitHubAppAuth() || cfg.Repos[0].Auth.PrivateKey != nil {
		t.Fatal("external token must never select App minting")
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.GitHubBotLogin("ACME", "WEB"); got != "qualification[bot]" {
		t.Fatalf("round-trip login = %q", got)
	}
	if got := cfg.GitHubBotLogin("acme", "other"); got != "" {
		t.Fatalf("external identity escaped its repository: %q", got)
	}
}

func TestExternalGitHubAppTokenValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*RepoRef)
		want string
	}{
		{"env", func(r *RepoRef) {}, ""},
		{"file", func(r *RepoRef) { r.Token = TokenRef{File: "/run/secrets/installation-token"} }, ""},
		{"keychain", func(r *RepoRef) { r.Token = TokenRef{Keychain: "installation-token"} }, ""},
		{"gh user", func(r *RepoRef) {
			r.Token = TokenRef{GitHubCLI: &GitHubCLIRef{Hostname: "github.com", User: "octocat"}}
		}, "githubCLI user identity is not supported"},
		{"missing token", func(r *RepoRef) { r.Token = TokenRef{} }, "requires exactly one token"},
		{"multiple refs", func(r *RepoRef) { r.Token.File = "/token" }, "exactly one"},
		{"missing slug", func(r *RepoRef) { r.Auth.Slug = "" }, "requires auth.slug"},
		{"suffix", func(r *RepoRef) { r.Auth.Slug = "app[bot]" }, "requires auth.slug"},
		{"whitespace", func(r *RepoRef) { r.Auth.Slug = " app " }, "requires auth.slug"},
		{"newline", func(r *RepoRef) { r.Auth.Slug = "app\n" }, "requires auth.slug"},
		{"unicode", func(r *RepoRef) { r.Auth.Slug = "аpp" }, "requires auth.slug"},
		{"long slug", func(r *RepoRef) { r.Auth.Slug = strings.Repeat("a", 40) }, "requires auth.slug"},
		{"app id", func(r *RepoRef) { r.Auth.AppID = "123" }, "must not configure"},
		{"installation id", func(r *RepoRef) { r.Auth.InstallationID = "123" }, "must not configure"},
		{"private key", func(r *RepoRef) { r.Auth.PrivateKey = &TokenRef{File: "/app.pem"} }, "must not configure"},
		{"tenant", func(r *RepoRef) { r.Auth.Tenant = "tenant" }, "only valid for ADO"},
		{"client", func(r *RepoRef) { r.Auth.ClientID = "client" }, "only valid for ADO"},
		{"ADO", func(r *RepoRef) { r.Provider, r.Project = "ado", "project" }, "only valid for provider"},
		{"Gitea", func(r *RepoRef) { r.Provider, r.BaseURL = "gitea", "https://example.com" }, "supports only a static token"},
		{"PAT slug remains rejected", func(r *RepoRef) { r.Auth.Kind = GitHubAuthPAT }, "only valid for auth kind"},
		{"unknown store", func(r *RepoRef) { r.Token = TokenRef{Store: "missing/token"} }, "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := RepoRef{Provider: "github", Owner: "acme", Name: "web",
				Token: TokenRef{Env: "TOKEN"}, Auth: &RepoAuthConfig{Kind: GitHubAuthAppToken, Slug: "my-app"}}
			tc.edit(&repo)
			cfg := &Config{Repos: []RepoRef{repo}}
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}
