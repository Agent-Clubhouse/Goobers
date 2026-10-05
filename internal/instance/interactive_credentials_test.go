package instance

import (
	"strings"
	"testing"
)

func TestInteractiveCredentialConfigValidation(t *testing.T) {
	valid := InteractiveCredential{Name: "human-code", Provider: "github", Owner: "org", Repository: "repo", Token: TokenRef{Env: "HUMAN_CODE_TOKEN"}}
	for _, tc := range []struct {
		name      string
		edit      func(*Config)
		wantError bool
	}{
		{"explicit PAT", func(c *Config) {}, false},
		{"ADO project only", func(c *Config) {
			c.InteractiveCredentials[0] = InteractiveCredential{Name: "human-backlog", Provider: "ado", Owner: "org", Project: "Work Project", Token: TokenRef{Env: "HUMAN_ADO_TOKEN"}}
		}, false},
		{"ADO azure CLI", func(c *Config) {
			c.InteractiveCredentials[0] = InteractiveCredential{Name: "human-backlog", Provider: "ado", Owner: "org", Project: "Work Project", Auth: &RepoAuthConfig{Kind: ADOAuthAzureCLI}}
		}, false},
		{"GitHub CLI foreign host", func(c *Config) {
			c.InteractiveCredentials[0].Token = TokenRef{GitHubCLI: &GitHubCLIRef{Hostname: "github.enterprise.example", User: "alice"}}
		}, true},
		{"duplicate", func(c *Config) { c.InteractiveCredentials = append(c.InteractiveCredentials, valid) }, true},
		{"missing token", func(c *Config) { c.InteractiveCredentials[0].Token = TokenRef{} }, true},
		{"ambiguous token", func(c *Config) { c.InteractiveCredentials[0].Token.File = "/secret" }, true},
		{"automation passthrough", func(c *Config) { c.Runner.EnvPassthrough = []string{"HUMAN_CODE_TOKEN"} }, true},
		{"foreign provider", func(c *Config) { c.InteractiveCredentials[0].Provider = "gitlab" }, true},
		{"github project", func(c *Config) { c.InteractiveCredentials[0].Project = "work" }, true},
		{"ADO GH CLI", func(c *Config) {
			c.InteractiveCredentials[0] = InteractiveCredential{Name: "human-backlog", Provider: "ado", Owner: "org", Project: "work", Token: TokenRef{GitHubCLI: &GitHubCLIRef{Hostname: "github.com", User: "alice"}}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{InteractiveCredentials: []InteractiveCredential{valid}}
			tc.edit(cfg)
			err := cfg.validateInteractiveCredentials(nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestLoadInteractiveCredentialsStrictSource(t *testing.T) {
	body := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token: {env: AUTOMATION_TOKEN}
interactiveCredentials:
  - name: human-backlog
    provider: ado
    owner: org
    project: Work Project
    token: {env: HUMAN_ADO_TOKEN}
`
	cfg, err := LoadConfig(writeInstanceYAML(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.InteractiveCredentials) != 1 || cfg.InteractiveCredentials[0].Repository != "" {
		t.Fatalf("sources=%+v", cfg.InteractiveCredentials)
	}
	for _, invalid := range []string{strings.Replace(body, "token: {env: HUMAN_ADO_TOKEN}", "token: literal-secret", 1), strings.Replace(body, "    project: Work Project", "    project: Work Project\n    arbitrary: value", 1)} {
		if _, err := LoadConfig(writeInstanceYAML(t, invalid)); err == nil {
			t.Fatal("accepted invalid interactive source")
		}
	}
}
