package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if cfg.APIListenAddress() != DefaultAPIListenAddress {
		t.Fatalf("APIListenAddress = %q, want %q", cfg.APIListenAddress(), DefaultAPIListenAddress)
	}
}

func TestLoadConfigAPIListenAddress(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
api:
  listen: "[::1]:9090"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.APIListenAddress(); got != "[::1]:9090" {
		t.Fatalf("APIListenAddress = %q, want [::1]:9090", got)
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

// TestLoadConfigCredentialsBlock is #287: instance.yaml's credentials: block
// parses into per-capability CredentialGrants, so an agentic stage can source
// agent:model from a personal Copilot-Requests PAT distinct from the repo token.
func TestLoadConfigCredentialsBlock(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GH_TOKEN
credentials:
  - capability: agent:model
    token:
      env: COPILOT_GITHUB_TOKEN
  - capability: repo:push
    token:
      file: /run/secrets/push-token
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Credentials) != 2 {
		t.Fatalf("expected 2 credentials, got %+v", cfg.Credentials)
	}
	if cfg.Credentials[0].Capability != "agent:model" || cfg.Credentials[0].Token.Env != "COPILOT_GITHUB_TOKEN" {
		t.Fatalf("unexpected credentials[0]: %+v", cfg.Credentials[0])
	}
	if cfg.Credentials[1].Capability != "repo:push" || cfg.Credentials[1].Token.File != "/run/secrets/push-token" {
		t.Fatalf("unexpected credentials[1]: %+v", cfg.Credentials[1])
	}
}

// TestLoadConfigCredentialsRejectsInlineSecret is #287's fail-closed guard: an
// inline value under a credentials token ref is an unknown field, rejected at
// load like a repo token's would be (CFG-009/SEC-010).
func TestLoadConfigCredentialsRejectsInlineSecret(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GH_TOKEN
credentials:
  - capability: agent:model
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
		{
			name:    "unresolvable timezone",
			cfg:     Config{Timezone: "Not/ARealZone"},
			wantErr: `timezone "Not/ARealZone"`,
		},
		{
			name: "valid timezone",
			cfg:  Config{Timezone: "America/New_York"},
		},
		{
			name:    "API wildcard host",
			cfg:     Config{API: APIConfig{Listen: ":8080"}},
			wantErr: "wildcard listeners are not allowed",
		},
		{
			name:    "API all interfaces",
			cfg:     Config{API: APIConfig{Listen: "0.0.0.0:8080"}},
			wantErr: "is not loopback",
		},
		{
			name:    "API non-loopback host",
			cfg:     Config{API: APIConfig{Listen: "example.com:8080"}},
			wantErr: "is not loopback",
		},
		{
			name: "API localhost",
			cfg:  Config{API: APIConfig{Listen: "localhost:8080"}},
		},
		{
			name: "API IPv4 loopback",
			cfg:  Config{API: APIConfig{Listen: "127.0.0.2:0"}},
		},
		{
			name: "API IPv6 loopback",
			cfg:  Config{API: APIConfig{Listen: "[::1]:8080"}},
		},
		{
			name:    "API invalid port",
			cfg:     Config{API: APIConfig{Listen: "127.0.0.1:70000"}},
			wantErr: "must be a number",
		},
		{
			name: "credentials unknown capability",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:mdoel", Token: TokenRef{Env: "T"}},
			}},
			wantErr: `unknown capability "agent:mdoel"`,
		},
		{
			name: "credentials duplicate capability",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:model", Token: TokenRef{Env: "A"}},
				{Capability: "agent:model", Token: TokenRef{File: "/b"}},
			}},
			wantErr: "sourced more than once",
		},
		{
			name: "credentials neither env nor file",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:model"},
			}},
			wantErr: "exactly one of env or file",
		},
		{
			name: "credentials both env and file",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:model", Token: TokenRef{Env: "T", File: "/f"}},
			}},
			wantErr: "exactly one of env or file",
		},
		{
			name: "credentials valid agent:model",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:model", Token: TokenRef{Env: "COPILOT_PAT"}},
			}},
		},
		{
			name: "credentials valid repo:push override",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "repo:push", Token: TokenRef{File: "/run/secrets/push-token"}},
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

// TestConfigLocation is issue #137's timezone-config wiring: Config.Location
// defaults to UTC when Timezone is unset (a fixed, reproducible default
// independent of the host process's own local zone) and resolves the
// configured IANA zone otherwise.
func TestConfigLocation(t *testing.T) {
	t.Run("defaults to UTC when unset", func(t *testing.T) {
		cfg := Config{}
		loc, err := cfg.Location()
		if err != nil {
			t.Fatalf("Location: %v", err)
		}
		if loc != time.UTC {
			t.Fatalf("Location = %v, want time.UTC", loc)
		}
	})
	t.Run("resolves the configured zone", func(t *testing.T) {
		if _, err := time.LoadLocation("America/New_York"); err != nil {
			t.Skipf("tzdata unavailable: %v", err)
		}
		cfg := Config{Timezone: "America/New_York"}
		loc, err := cfg.Location()
		if err != nil {
			t.Fatalf("Location: %v", err)
		}
		if loc.String() != "America/New_York" {
			t.Fatalf("Location = %v, want America/New_York", loc)
		}
	})
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
