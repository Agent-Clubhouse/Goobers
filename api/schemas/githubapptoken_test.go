package schemas

import (
	"fmt"
	"testing"
)

func TestInstanceSchemaExternalGitHubAppToken(t *testing.T) {
	schema := compileInstanceSchema(t)
	cases := []struct {
		name, provider, token, auth string
		valid                       bool
	}{
		{"env", "github", "token: {env: INSTALLATION_TOKEN}", "slug: my-app", true},
		{"file", "github", "token: {file: /token}", "slug: my-app", true},
		{"missing token", "github", "", "slug: my-app", false},
		{"missing slug", "github", "token: {env: TOKEN}", "", false},
		{"suffix", "github", "token: {env: TOKEN}", "slug: 'my-app[bot]'", false},
		{"private key", "github", "token: {env: TOKEN}", "slug: my-app\n      privateKey: {file: /app.pem}", false},
		{"gh user", "github", "token: {githubCLI: {hostname: github.com, user: octocat}}", "slug: my-app", false},
		{"ADO", "ado", "token: {env: TOKEN}\n    project: project", "slug: my-app", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: %s
    owner: acme
    name: web
    %s
    auth:
      kind: github-app-token
      %s
`, tc.provider, tc.token, tc.auth)
			if err := validateInstanceYAML(t, schema, document); (err == nil) != tc.valid {
				t.Fatalf("schema validation = %v, valid = %v", err, tc.valid)
			}
		})
	}
}
