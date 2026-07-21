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
  stalledRunTimeout: 30m
  claimsLockTimeout: 15s
retention:
  enabled: true
  dryRun: true
  maxRetainedWorktreeBytes: 1048576
  retainedWorktreeMaxAge: 72h
notifications: true
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
	if got, err := cfg.RunConditions.StalledRunTimeoutDuration(); err != nil || got != 30*time.Minute {
		t.Fatalf("StalledRunTimeoutDuration = %s, %v; want 30m", got, err)
	}
	if got, err := cfg.RunConditions.ClaimsLockTimeoutDuration(); err != nil || got != 15*time.Second {
		t.Fatalf("ClaimsLockTimeoutDuration = %s, %v; want 15s", got, err)
	}
	if !cfg.Notifications {
		t.Fatal("expected notifications to be enabled")
	}
	if !cfg.Retention.Enabled || !cfg.Retention.DryRun || cfg.Retention.MaxRetainedWorktreeBytes != 1048576 {
		t.Fatalf("unexpected retention config: %+v", cfg.Retention)
	}
	if got, err := cfg.Retention.RetainedWorktreeMaxAgeDuration(); err != nil || got != 72*time.Hour {
		t.Fatalf("RetainedWorktreeMaxAgeDuration = %s, %v; want 72h", got, err)
	}
	if cfg.APIListenAddress() != DefaultAPIListenAddress {
		t.Fatalf("APIListenAddress = %q, want %q", cfg.APIListenAddress(), DefaultAPIListenAddress)
	}
}

func TestRetentionConfigDefaultsDisabledAndValidatesLimits(t *testing.T) {
	var zero RetentionConfig
	if zero.Enabled || zero.DryRun || zero.MaxRetainedWorktreeBytes != 0 {
		t.Fatalf("zero retention config is not disabled: %+v", zero)
	}
	if got, err := zero.RetainedWorktreeMaxAgeDuration(); err != nil || got != 0 {
		t.Fatalf("default RetainedWorktreeMaxAgeDuration = %s, %v; want 0, nil", got, err)
	}

	for _, cfg := range []RetentionConfig{
		{MaxRetainedWorktreeBytes: -1},
		{RetainedWorktreeMaxAge: "not-a-duration"},
		{RetainedWorktreeMaxAge: "0s"},
		{RetainedWorktreeMaxAge: "-1h"},
	} {
		if err := (&Config{Retention: cfg}).Validate(); err == nil || !strings.Contains(err.Error(), "retention.") {
			t.Fatalf("Validate(%+v) error = %v, want retention error", cfg, err)
		}
	}
}

func TestStalledRunTimeout(t *testing.T) {
	if got, err := (RunConditions{}).StalledRunTimeoutDuration(); err != nil || got != DefaultStalledRunTimeout {
		t.Fatalf("default StalledRunTimeoutDuration = %s, %v; want %s", got, err, DefaultStalledRunTimeout)
	}
	if got, err := (RunConditions{StalledRunTimeout: "1ns"}).StalledRunTimeoutDuration(); err != nil || got != time.Nanosecond {
		t.Fatalf("shortest StalledRunTimeoutDuration = %s, %v; want 1ns", got, err)
	}
	for _, value := range []string{"not-a-duration", "0s", "-1m"} {
		t.Run(value, func(t *testing.T) {
			cfg := Config{RunConditions: RunConditions{StalledRunTimeout: value}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "stalledRunTimeout") {
				t.Fatalf("Validate() error = %v, want stalledRunTimeout error", err)
			}
		})
	}
}

func TestClaimsLockTimeout(t *testing.T) {
	if got, err := (RunConditions{}).ClaimsLockTimeoutDuration(); err != nil || got != DefaultClaimsLockTimeout {
		t.Fatalf("default ClaimsLockTimeoutDuration = %s, %v; want %s", got, err, DefaultClaimsLockTimeout)
	}
	if got, err := (RunConditions{ClaimsLockTimeout: "1ns"}).ClaimsLockTimeoutDuration(); err != nil || got != time.Nanosecond {
		t.Fatalf("shortest ClaimsLockTimeoutDuration = %s, %v; want 1ns", got, err)
	}
	for _, value := range []string{"not-a-duration", "0s", "-1m"} {
		t.Run(value, func(t *testing.T) {
			cfg := Config{RunConditions: RunConditions{ClaimsLockTimeout: value}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "claimsLockTimeout") {
				t.Fatalf("Validate() error = %v, want claimsLockTimeout error", err)
			}
		})
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

func TestLoadConfigWebhook(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
webhook:
  listen: "[::1]:9091"
  secret:
    env: GITHUB_WEBHOOK_SECRET
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.WebhookListenAddress(); got != "[::1]:9091" {
		t.Fatalf("WebhookListenAddress = %q, want [::1]:9091", got)
	}
	if !cfg.WebhookSecretConfigured() || cfg.Webhook.Secret.Env != "GITHUB_WEBHOOK_SECRET" {
		t.Fatalf("unexpected webhook secret ref: %+v", cfg.Webhook.Secret)
	}
}

func TestLoadConfigOTLP(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
telemetry:
  otlp:
    endpoint: https://collector.example.com:4317
    headers:
      authorization:
        env: GOOBERS_OTLP_AUTHORIZATION
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Telemetry.OTLP.Endpoint != "https://collector.example.com:4317" {
		t.Fatalf("unexpected OTLP endpoint: %q", cfg.Telemetry.OTLP.Endpoint)
	}
	if got := cfg.Telemetry.OTLP.Headers["authorization"].Env; got != "GOOBERS_OTLP_AUTHORIZATION" {
		t.Fatalf("authorization env = %q, want GOOBERS_OTLP_AUTHORIZATION", got)
	}
}

func TestLoadConfigOTLPEnvironmentOverridesFile(t *testing.T) {
	t.Setenv(OTLPEndpointEnv, "https://collector.example.com:443")
	t.Setenv(OTLPInsecureEnv, "false")
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
telemetry:
  otlp:
    endpoint: http://127.0.0.1:4317
    insecure: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Telemetry.OTLP.Endpoint != "https://collector.example.com:443" || cfg.Telemetry.OTLP.Insecure {
		t.Fatalf("resolved OTLP config = %+v, want environment endpoint with TLS", cfg.Telemetry.OTLP)
	}
}

func TestLoadConfigOTLPRejectsInlineHeaderSecret(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
telemetry:
  otlp:
    endpoint: https://collector.example.com:4317
    headers:
      authorization:
        value: Bearer secret
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected inline header value to be rejected, got %v", err)
	}
}

func TestResolveOTLPConfig(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		cfg := Config{}
		resolved, err := cfg.ResolveOTLPConfig(func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Enabled() {
			t.Fatalf("OTLP push enabled with empty configuration: %+v", resolved)
		}
	})

	t.Run("environment overrides file", func(t *testing.T) {
		cfg := Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
			Endpoint: "http://127.0.0.1:4317",
			Insecure: true,
		}}}
		env := map[string]string{
			OTLPEndpointEnv: "https://collector.example.com:443",
			OTLPInsecureEnv: "false",
		}
		resolved, err := cfg.ResolveOTLPConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Endpoint != env[OTLPEndpointEnv] || resolved.Insecure {
			t.Fatalf("resolved OTLP config = %+v, want environment endpoint with TLS", resolved)
		}
	})

	t.Run("environment can opt in", func(t *testing.T) {
		cfg := Config{}
		env := map[string]string{
			OTLPEndpointEnv: "http://localhost:4317",
			OTLPInsecureEnv: "true",
		}
		resolved, err := cfg.ResolveOTLPConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		})
		if err != nil {
			t.Fatal(err)
		}
		if !resolved.Enabled() || !resolved.Insecure {
			t.Fatalf("resolved OTLP config = %+v, want enabled insecure loopback", resolved)
		}
	})

	t.Run("invalid environment fails closed", func(t *testing.T) {
		cfg := Config{}
		_, err := cfg.ResolveOTLPConfig(func(key string) (string, bool) {
			if key == OTLPInsecureEnv {
				return "sometimes", true
			}
			return "", false
		})
		if err == nil || !strings.Contains(err.Error(), OTLPInsecureEnv+" must be true or false") {
			t.Fatalf("expected invalid environment error, got %v", err)
		}
	})
}

func TestOTLPConfigValidatesGRPCMetadataNames(t *testing.T) {
	valid := OTLPConfig{
		Endpoint: "https://collector.example.com:4317",
		Headers:  map[string]TokenRef{"X.Trace_ID-1": {Env: "OTLP_TRACE_ID"}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid gRPC metadata name rejected: %v", err)
	}

	for _, name := range []string{
		"x-api+key",
		"x-api!key",
		"x-api~key",
		"grpc-timeout",
		"GRPC-custom",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := OTLPConfig{
				Endpoint: "https://collector.example.com:4317",
				Headers:  map[string]TokenRef{name: {Env: "OTLP_HEADER"}},
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "invalid header name") {
				t.Fatalf("expected invalid gRPC metadata name error, got %v", err)
			}
		})
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
			name:    "webhook all interfaces",
			cfg:     Config{Webhook: WebhookConfig{Listen: "0.0.0.0:8081"}},
			wantErr: "webhook.listen",
		},
		{
			name: "webhook secret both env and file",
			cfg: Config{Webhook: WebhookConfig{
				Secret: TokenRef{Env: "WEBHOOK_SECRET", File: "/run/secrets/webhook"},
			}},
			wantErr: "webhook.secret must reference exactly one",
		},
		{
			name: "webhook loopback and env secret",
			cfg: Config{Webhook: WebhookConfig{
				Listen: "127.0.0.2:0",
				Secret: TokenRef{Env: "WEBHOOK_SECRET"},
			}},
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
		{
			name: "runner capabilities valid free-form tokens",
			cfg:  Config{Runner: RunnerConfig{Capabilities: []string{"dotnet@8", "xcode", "os=windows"}}},
		},
		{
			name:    "runner capability malformed rejected",
			cfg:     Config{Runner: RunnerConfig{Capabilities: []string{"dotnet 8"}}},
			wantErr: "runner.capabilities[0]",
		},
		{
			name:    "runner capability empty rejected",
			cfg:     Config{Runner: RunnerConfig{Capabilities: []string{"dotnet@8", ""}}},
			wantErr: "runner.capabilities[1]",
		},
		{
			name: "runner env passthrough valid names",
			cfg:  Config{Runner: RunnerConfig{EnvPassthrough: []string{"DOTNET_ROOT", "MY_TOOL_HOME", "npm_config_cache"}}},
		},
		{
			name:    "runner env passthrough with assignment rejected",
			cfg:     Config{Runner: RunnerConfig{EnvPassthrough: []string{"FOO=BAR"}}},
			wantErr: "runner.envPassthrough[0]",
		},
		{
			name:    "runner env passthrough empty rejected",
			cfg:     Config{Runner: RunnerConfig{EnvPassthrough: []string{"DOTNET_ROOT", ""}}},
			wantErr: "runner.envPassthrough[1]",
		},
		{
			name:    "runner env passthrough leading digit rejected",
			cfg:     Config{Runner: RunnerConfig{EnvPassthrough: []string{"1BAD"}}},
			wantErr: "runner.envPassthrough[0]",
		},
		{
			name: "OTLP secure endpoint",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "https://collector.example.com:4317",
				Headers:  map[string]TokenRef{"authorization": {Env: "OTLP_AUTHORIZATION"}},
			}}},
		},
		{
			name: "OTLP secure host port endpoint",
			cfg:  Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{Endpoint: "collector.example.com:4317"}}},
		},
		{
			name: "OTLP insecure loopback",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "http://127.0.0.1:4317",
				Insecure: true,
			}}},
		},
		{
			name: "OTLP insecure remote",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "http://collector.example.com:4317",
				Insecure: true,
			}}},
			wantErr: "insecure mode is allowed only",
		},
		{
			name:    "OTLP http without insecure",
			cfg:     Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{Endpoint: "http://localhost:4317"}}},
			wantErr: "http requires explicit insecure",
		},
		{
			name: "OTLP https with insecure",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "https://localhost:4317",
				Insecure: true,
			}}},
			wantErr: "https conflicts with insecure",
		},
		{
			name: "OTLP header without source",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "https://collector.example.com:4317",
				Headers:  map[string]TokenRef{"authorization": {}},
			}}},
			wantErr: "must reference exactly one of env or file",
		},
		{
			name: "OTLP ambiguous header source",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "https://collector.example.com:4317",
				Headers: map[string]TokenRef{
					"authorization": {Env: "AUTH", File: "/run/secrets/auth"},
				},
			}}},
			wantErr: "must reference exactly one of env or file",
		},
		{
			name: "OTLP settings without endpoint",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Insecure: true,
			}}},
			wantErr: "endpoint is required",
		},
		{
			name: "OTLP disabled telemetry conflict",
			cfg: Config{Telemetry: TelemetryConfig{
				Enabled: boolConfig(false),
				OTLP:    &OTLPConfig{Endpoint: "https://collector.example.com:4317"},
			}},
			wantErr: "cannot be set when telemetry.enabled is false",
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

func boolConfig(value bool) *bool {
	return &value
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
		RunConditions: RunConditions{StalledRunTimeout: "20m"},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "otlp:") {
		t.Fatalf("disabled OTLP configuration should be omitted, got:\n%s", raw)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Repos) != 1 || got.Repos[0].Token.Env != "GITHUB_TOKEN" {
		t.Fatalf("round-trip mismatch: %+v", got.Repos)
	}
	if got.RunConditions.StalledRunTimeout != "20m" {
		t.Fatalf("stalledRunTimeout = %q, want 20m", got.RunConditions.StalledRunTimeout)
	}
}
