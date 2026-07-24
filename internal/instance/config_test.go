package instance

import (
	"os"
	"path/filepath"
	"reflect"
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
runner:
  livenessTimeout: 3m
telemetry:
  retention:
    enabled: true
    window: 30d
    maxRuns: 25
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
	if cfg.Telemetry.Retention == nil || !cfg.Telemetry.Retention.Enabled ||
		cfg.Telemetry.Retention.Window != "30d" || cfg.Telemetry.Retention.MaxRuns != 25 {
		t.Fatalf("unexpected telemetry retention config: %+v", cfg.Telemetry.Retention)
	}
	if got, err := cfg.Telemetry.Retention.WindowDuration(); err != nil || got != 30*24*time.Hour {
		t.Fatalf("telemetry WindowDuration = %s, %v; want 30d", got, err)
	}
	if cfg.RunConditions.MaxParallelRuns != 2 {
		t.Fatalf("expected maxParallelRuns=2, got %d", cfg.RunConditions.MaxParallelRuns)
	}
	if got, err := cfg.Runner.LivenessTimeoutDuration(); err != nil || got != 3*time.Minute {
		t.Fatalf("LivenessTimeoutDuration = %s, %v; want 3m", got, err)
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

func TestLoadConfigWorkflowSource(t *testing.T) {
	tests := []struct {
		name       string
		sourceYAML string
		want       WorkflowSource
		wantRef    string
	}{
		{
			name: "local directory",
			sourceYAML: `
  kind: local-dir
  path: ../workflow-config
`,
			want:    WorkflowSource{Kind: "local-dir", Path: "../workflow-config"},
			wantRef: "",
		},
		{
			name: "local git repository",
			sourceYAML: `
  kind: git
  path: ../workflow-config
  ref: release
`,
			want:    WorkflowSource{Kind: "git", Path: "../workflow-config", Ref: "release"},
			wantRef: "release",
		},
		{
			name: "remote git repository defaults to main",
			sourceYAML: `
  kind: git
  url: https://github.com/acme/workflows.git
  token:
    env: WORKFLOW_CONFIG_TOKEN
`,
			want: WorkflowSource{
				Kind:  "git",
				URL:   "https://github.com/acme/workflows.git",
				Token: &TokenRef{Env: "WORKFLOW_CONFIG_TOKEN"},
			},
			wantRef: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: application
    token:
      env: CODE_REPO_TOKEN
workflowSource:
`+tt.sourceYAML)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.WorkflowSource == nil {
				t.Fatal("WorkflowSource is nil")
			}
			if got := *cfg.WorkflowSource; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("WorkflowSource = %+v, want %+v", got, tt.want)
			}
			if got := cfg.WorkflowSource.TrackedRef(); got != tt.wantRef {
				t.Fatalf("TrackedRef = %q, want %q", got, tt.wantRef)
			}
			if len(cfg.Repos) != 1 || cfg.Repos[0].Token.Env != "CODE_REPO_TOKEN" {
				t.Fatalf("workflow source changed target repos: %+v", cfg.Repos)
			}
		})
	}
}

func TestWorkflowSourceValidation(t *testing.T) {
	tests := []struct {
		name    string
		source  WorkflowSource
		wantErr string
	}{
		{
			name:    "unknown kind",
			source:  WorkflowSource{Kind: "filesystem", Path: "config"},
			wantErr: "unsupported kind",
		},
		{
			name:    "local directory missing path",
			source:  WorkflowSource{Kind: "local-dir"},
			wantErr: "path is required",
		},
		{
			name:    "local directory with git field",
			source:  WorkflowSource{Kind: "local-dir", Path: "config", Ref: "main"},
			wantErr: "accepts only path",
		},
		{
			name:    "git missing location",
			source:  WorkflowSource{Kind: "git"},
			wantErr: "exactly one of path or url",
		},
		{
			name:    "git has path and url",
			source:  WorkflowSource{Kind: "git", Path: "config", URL: "https://example.com/config.git"},
			wantErr: "exactly one of path or url",
		},
		{
			name: "remote git missing token",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
			},
			wantErr: "remote git token must reference exactly one",
		},
		{
			name: "remote git token has env and file",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "https://example.com/config.git",
				Token: &TokenRef{Env: "CONFIG_TOKEN", File: "/run/secrets/config-token"},
			},
			wantErr: "remote git token must reference exactly one",
		},
		{
			name: "remote git file url",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "file:///tmp/config.git",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
			},
			wantErr: "must use https",
		},
		{
			name: "remote git ssh url",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "ssh://git@example.com/config.git",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
			},
			wantErr: "must use https",
		},
		{
			name: "remote git scp url",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "git@example.com:config.git",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
			},
			wantErr: "must use https",
		},
		{
			name: "remote git url with userinfo",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "https://user:password@example.com/config.git",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
			},
			wantErr: "must not include userinfo",
		},
		{
			name: "remote git url with credential query",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "https://example.com/config.git?token=secret",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
			},
			wantErr: "must not include a query or fragment",
		},
		{
			name: "local git with token",
			source: WorkflowSource{
				Kind:  "git",
				Path:  "config",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
			},
			wantErr: "token is only valid for a remote git url",
		},
		{
			name:    "location with surrounding whitespace",
			source:  WorkflowSource{Kind: "local-dir", Path: " config"},
			wantErr: "path must not contain leading or trailing whitespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{WorkflowSource: &tt.source}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigRejectsWorkflowSourceTokenExposedToStages(t *testing.T) {
	tests := []struct {
		name  string
		token string
		extra []string
	}{
		{
			name:  "explicit passthrough",
			token: "WORKFLOW_SOURCE_TOKEN",
			extra: []string{"WORKFLOW_SOURCE_TOKEN"},
		},
		{
			name:  "built-in exact allowlist",
			token: "HOME",
		},
		{
			name:  "built-in prefix allowlist",
			token: "LC_WORKFLOW_SOURCE_TOKEN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				WorkflowSource: &WorkflowSource{
					Kind:  WorkflowSourceKindGit,
					URL:   "https://example.com/config.git",
					Token: &TokenRef{Env: tt.token},
				},
				Runner: RunnerConfig{EnvPassthrough: tt.extra},
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "must not be exposed to stages") {
				t.Fatalf("Validate error = %v, want workflow-source token exposure rejection", err)
			}
		})
	}
}

func TestConfigRejectsConfigRepoReadInStageCredentials(t *testing.T) {
	cfg := Config{Credentials: []CredentialGrant{{
		Capability: "configrepo:read",
		Token:      TokenRef{Env: "CD_PAT"},
	}}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `capability "configrepo:read" is runner-owned`) {
		t.Fatalf("Validate error = %v, want runner-owned credential rejection", err)
	}
}

func TestLoadConfigRejectsInlineWorkflowSourceToken(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
workflowSource:
  kind: git
  url: https://github.com/acme/workflows.git
  token:
    value: ghp_inlinesecrettoken
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected inline workflow source token to be rejected, got %v", err)
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

func TestTelemetryRetentionConfigDefaultsAndValidatesLimits(t *testing.T) {
	var zero TelemetryRetentionConfig
	if zero.Enabled {
		t.Fatal("zero telemetry retention config must disable automatic pruning")
	}
	if got, err := zero.WindowDuration(); err != nil || got != DefaultTelemetryRetentionWindow {
		t.Fatalf("default WindowDuration = %s, %v; want %s", got, err, DefaultTelemetryRetentionWindow)
	}
	if got := zero.MaxRunLimit(); got != DefaultTelemetryRetentionMaxRuns {
		t.Fatalf("default MaxRunLimit = %d, want %d", got, DefaultTelemetryRetentionMaxRuns)
	}

	for _, cfg := range []TelemetryRetentionConfig{
		{Window: "not-a-duration"},
		{Window: "0d"},
		{Window: "-1h"},
		{MaxRuns: -1},
	} {
		if err := (&Config{Telemetry: TelemetryConfig{Retention: &cfg}}).Validate(); err == nil ||
			!strings.Contains(err.Error(), "telemetry.retention.") {
			t.Fatalf("Validate(%+v) error = %v, want telemetry retention error", cfg, err)
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

func TestDaemonLivenessTimeout(t *testing.T) {
	if got, err := (RunnerConfig{}).LivenessTimeoutDuration(); err != nil || got != DefaultDaemonLivenessTimeout {
		t.Fatalf("default LivenessTimeoutDuration = %s, %v; want %s", got, err, DefaultDaemonLivenessTimeout)
	}
	if got, err := (RunnerConfig{LivenessTimeout: "10s"}).LivenessTimeoutDuration(); err != nil || got != 10*time.Second {
		t.Fatalf("LivenessTimeoutDuration = %s, %v; want 10s", got, err)
	}
	for _, value := range []string{"not-a-duration", "0s", "1s", "-1m"} {
		t.Run(value, func(t *testing.T) {
			cfg := Config{Runner: RunnerConfig{LivenessTimeout: value}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "livenessTimeout") {
				t.Fatalf("Validate() error = %v, want livenessTimeout error", err)
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
				{Provider: "gitlab", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}},
			}},
			wantErr: "unsupported provider",
		},
		{
			name: "valid ado PAT",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Token: TokenRef{Env: "T"}},
			}},
		},
		{
			name: "valid ado Azure CLI",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Auth: &ADOAuthConfig{Kind: ADOAuthAzureCLI}},
			}},
		},
		{
			name: "ado missing project",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Name: "web", Auth: &ADOAuthConfig{Kind: ADOAuthAzureCLI}},
			}},
			wantErr: "project is required",
		},
		{
			name: "ado identity auth rejects PAT",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Token: TokenRef{Env: "T"}, Auth: &ADOAuthConfig{Kind: ADOAuthWorkloadIdentity}},
			}},
			wantErr: "must not configure token",
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
			wantErr: "exactly one of env, file, or store",
		},
		{
			name: "both env and file",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T", File: "/f"}},
			}},
			wantErr: "exactly one of env, file, or store",
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
			wantErr: "exactly one of env, file, or store",
		},
		{
			name: "credentials both env and file",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:model", Token: TokenRef{Env: "T", File: "/f"}},
			}},
			wantErr: "exactly one of env, file, or store",
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
			wantErr: "must reference exactly one of env, file, or store",
		},
		{
			name: "OTLP ambiguous header source",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "https://collector.example.com:4317",
				Headers: map[string]TokenRef{
					"authorization": {Env: "AUTH", File: "/run/secrets/auth"},
				},
			}}},
			wantErr: "must reference exactly one of env, file, or store",
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

// validSecretStore returns a well-formed secretStores entry tests mutate.
func validSecretStore() SecretStoreConfig {
	return SecretStoreConfig{
		Name:     "prod-kv",
		Kind:     SecretStoreKindAzureKeyVault,
		VaultURI: "https://acme.vault.azure.net",
		Auth:     &SecretStoreAuthConfig{Kind: SecretStoreAuthWorkloadIdentity},
	}
}

func TestLoadConfigSecretStores(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
secretStores:
  - name: prod-kv
    kind: azure-key-vault
    vaultURI: https://acme.vault.azure.net
    auth:
      kind: managed-identity
      clientId: 00000000-0000-0000-0000-000000000000
    cacheTTLSeconds: 300
repos:
  - provider: github
    owner: acme
    name: web
    token:
      store: prod-kv/github-token
credentials:
  - capability: agent:model
    token:
      store: prod-kv/copilot-token
webhook:
  secret:
    store: prod-kv/webhook-secret
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.SecretStores) != 1 || cfg.SecretStores[0].Name != "prod-kv" {
		t.Fatalf("unexpected secretStores: %+v", cfg.SecretStores)
	}
	if cfg.SecretStores[0].CacheTTLSeconds != 300 {
		t.Fatalf("cacheTTLSeconds = %d, want 300", cfg.SecretStores[0].CacheTTLSeconds)
	}
	if cfg.Repos[0].Token.Store != "prod-kv/github-token" {
		t.Fatalf("repo token store = %q", cfg.Repos[0].Token.Store)
	}
	if !cfg.WebhookSecretConfigured() {
		t.Fatal("store-backed webhook secret must count as configured")
	}
}

func TestConfigValidateSecretStores(t *testing.T) {
	// storeConfig builds a Config carrying one secret store entry mutated by
	// each case, plus a store-free repo so store errors are isolated.
	storeConfig := func(mutate func(*SecretStoreConfig)) Config {
		store := validSecretStore()
		mutate(&store)
		return Config{SecretStores: []SecretStoreConfig{store}}
	}
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "valid store",
			cfg:  storeConfig(func(*SecretStoreConfig) {}),
		},
		{
			name: "valid azure-cli auth",
			cfg: storeConfig(func(s *SecretStoreConfig) {
				s.Auth = &SecretStoreAuthConfig{Kind: SecretStoreAuthAzureCLI}
			}),
		},
		{
			name:    "missing name",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.Name = "" }),
			wantErr: "secretStores[0]: name is required",
		},
		{
			name:    "uppercase name rejected",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.Name = "Prod-KV" }),
			wantErr: "must be a lowercase DNS label",
		},
		{
			name:    "name with slash rejected",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.Name = "prod/kv" }),
			wantErr: "must be a lowercase DNS label",
		},
		{
			name: "duplicate name",
			cfg: Config{SecretStores: []SecretStoreConfig{
				validSecretStore(), validSecretStore(),
			}},
			wantErr: "declared more than once",
		},
		{
			name:    "unsupported kind",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.Kind = "hashicorp-vault" }),
			wantErr: `unsupported kind "hashicorp-vault"`,
		},
		{
			name:    "missing vaultURI",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.VaultURI = "" }),
			wantErr: "vaultURI: is required",
		},
		{
			name:    "http vaultURI rejected",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.VaultURI = "http://acme.vault.azure.net" }),
			wantErr: "scheme must be https",
		},
		{
			name:    "vaultURI with path rejected",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.VaultURI = "https://acme.vault.azure.net/secrets" }),
			wantErr: "paths, queries, and fragments are not supported",
		},
		{
			name:    "missing auth",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.Auth = nil }),
			wantErr: "auth is required",
		},
		{
			name: "unsupported auth kind",
			cfg: storeConfig(func(s *SecretStoreConfig) {
				s.Auth = &SecretStoreAuthConfig{Kind: "pat"}
			}),
			wantErr: `unsupported auth kind "pat"`,
		},
		{
			name: "azure-cli auth rejects clientId",
			cfg: storeConfig(func(s *SecretStoreConfig) {
				s.Auth = &SecretStoreAuthConfig{Kind: SecretStoreAuthAzureCLI, ClientID: "abc"}
			}),
			wantErr: "auth.clientId is not valid",
		},
		{
			name:    "negative cache TTL",
			cfg:     storeConfig(func(s *SecretStoreConfig) { s.CacheTTLSeconds = -1 }),
			wantErr: "cacheTTLSeconds must not be negative",
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

func TestConfigValidateStoreRefs(t *testing.T) {
	withStore := func(mutate func(*Config)) Config {
		cfg := Config{SecretStores: []SecretStoreConfig{validSecretStore()}}
		mutate(&cfg)
		return cfg
	}
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "repo token store ref valid",
			cfg: withStore(func(c *Config) {
				c.Repos = []RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Store: "prod-kv/github-token"}}}
			}),
		},
		{
			name: "repo token env and store rejected",
			cfg: withStore(func(c *Config) {
				c.Repos = []RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T", Store: "prod-kv/github-token"}}}
			}),
			wantErr: "exactly one of env, file, or store",
		},
		{
			name: "store ref without separator rejected",
			cfg: withStore(func(c *Config) {
				c.Repos = []RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Store: "github-token"}}}
			}),
			wantErr: `must have the form "<storeName>/<secretName>"`,
		},
		{
			name: "store ref with extra separator rejected",
			cfg: withStore(func(c *Config) {
				c.Repos = []RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Store: "prod-kv/a/b"}}}
			}),
			wantErr: `must have the form "<storeName>/<secretName>"`,
		},
		{
			name: "store ref with empty secret rejected",
			cfg: withStore(func(c *Config) {
				c.Repos = []RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Store: "prod-kv/"}}}
			}),
			wantErr: `must have the form "<storeName>/<secretName>"`,
		},
		{
			name: "undeclared store rejected",
			cfg: withStore(func(c *Config) {
				c.Repos = []RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Store: "staging-kv/github-token"}}}
			}),
			wantErr: "not declared under secretStores",
		},
		{
			name: "store ref without any declared stores rejected",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Store: "prod-kv/github-token"}},
			}},
			wantErr: "not declared under secretStores",
		},
		{
			name: "credentials store ref valid",
			cfg: withStore(func(c *Config) {
				c.Credentials = []CredentialGrant{{Capability: "agent:model", Token: TokenRef{Store: "prod-kv/copilot"}}}
			}),
		},
		{
			name: "credentials undeclared store rejected",
			cfg: withStore(func(c *Config) {
				c.Credentials = []CredentialGrant{{Capability: "agent:model", Token: TokenRef{Store: "other/copilot"}}}
			}),
			wantErr: "not declared under secretStores",
		},
		{
			name: "webhook undeclared store rejected",
			cfg: withStore(func(c *Config) {
				c.Webhook = WebhookConfig{Secret: TokenRef{Store: "other/webhook"}}
			}),
			wantErr: "not declared under secretStores",
		},
		{
			name: "otlp header store ref valid",
			cfg: withStore(func(c *Config) {
				c.Telemetry = TelemetryConfig{OTLP: &OTLPConfig{
					Endpoint: "https://collector.example.com:4317",
					Headers:  map[string]TokenRef{"authorization": {Store: "prod-kv/otlp-auth"}},
				}}
			}),
		},
		{
			name: "otlp header undeclared store rejected",
			cfg: withStore(func(c *Config) {
				c.Telemetry = TelemetryConfig{OTLP: &OTLPConfig{
					Endpoint: "https://collector.example.com:4317",
					Headers:  map[string]TokenRef{"authorization": {Store: "other/otlp-auth"}},
				}}
			}),
			wantErr: "not declared under secretStores",
		},
		{
			name: "workflow source store ref valid",
			cfg: withStore(func(c *Config) {
				c.WorkflowSource = &WorkflowSource{
					Kind:  WorkflowSourceKindGit,
					URL:   "https://github.com/acme/config.git",
					Token: &TokenRef{Store: "prod-kv/config-token"},
				}
			}),
		},
		{
			name: "workflow source undeclared store rejected",
			cfg: withStore(func(c *Config) {
				c.WorkflowSource = &WorkflowSource{
					Kind:  WorkflowSourceKindGit,
					URL:   "https://github.com/acme/config.git",
					Token: &TokenRef{Store: "other/config-token"},
				}
			}),
			wantErr: "not declared under secretStores",
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

// TestTokenRefEnvFileSources pins the fail-closed contract every local-only
// consumer relies on (#683): a store-backed ref must error, never read as an
// unconfigured ref, until a build path wires real store resolution.
func TestTokenRefEnvFileSources(t *testing.T) {
	env, file, err := TokenRef{Env: "T"}.EnvFileSources()
	if err != nil || env != "T" || file != "" {
		t.Fatalf("env ref = (%q, %q, %v)", env, file, err)
	}
	if _, _, err := (TokenRef{Store: "prod-kv/github-token"}).EnvFileSources(); err == nil ||
		!strings.Contains(err.Error(), "secret store resolution is not configured in this build path yet") {
		t.Fatalf("store ref must fail closed, got %v", err)
	}
}
