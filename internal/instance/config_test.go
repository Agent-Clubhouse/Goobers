package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstanceYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write instance.yaml: %v", err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
runConditions:
  maxParallelRuns: 2
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "acme" {
		t.Fatalf("unexpected repos: %+v", cfg.Repos)
	}
	if cfg.Repos[0].Token.Env != "GITHUB_TOKEN" {
		t.Fatalf("expected token.env, got %+v", cfg.Repos[0].Token)
	}
	if !cfg.TelemetryEnabled() {
		t.Fatalf("expected telemetry enabled by default")
	}
	if cfg.RunConditions.MaxParallelRuns != 2 {
		t.Fatalf("expected maxParallelRuns=2, got %d", cfg.RunConditions.MaxParallelRuns)
	}
}

func TestLoadConfigFileTokenRef(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      file: /run/secrets/github-token
`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

func TestLoadConfigRejectsInlineSecret(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      value: ghp_inlinesecrettoken
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected an error for an inline secret value, got nil")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected an unknown-field error, got: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "unsupported provider",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}},
			}},
			wantErr: "unsupported provider",
		},
		{
			name: "missing owner",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Name: "web", Token: TokenRef{Env: "T"}},
			}},
			wantErr: "owner and name are required",
		},
		{
			name: "neither env nor file",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web"},
			}},
			wantErr: "exactly one of env or file",
		},
		{
			name: "both env and file",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T", File: "/f"}},
			}},
			wantErr: "exactly one of env or file",
		},
		{
			name: "valid",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestWriteConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	cfg := &Config{
		APIVersion: ConfigAPIVersion,
		Kind:       ConfigKind,
		Repos: []RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "GITHUB_TOKEN"}},
		},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Repos) != 1 || got.Repos[0].Token.Env != "GITHUB_TOKEN" {
		t.Fatalf("round-trip mismatch: %+v", got.Repos)
	}
}
