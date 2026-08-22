package instance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/externaltelemetry"
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
speech:
  enabled: true
  engine: say
  voice: Samantha
  language: en-US
  rate: 210
  timeout: 8s
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
	if cfg.Speech == nil || !cfg.Speech.Enabled || cfg.Speech.Engine != "say" || cfg.Speech.Voice != "Samantha" ||
		cfg.Speech.Language != "en-US" || cfg.Speech.Rate != 210 || cfg.Speech.Timeout != "8s" {
		t.Fatalf("unexpected speech config: %+v", cfg.Speech)
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

func TestLoadConfigRejectsInvalidSpeechConfig(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
speech:
  enabled: true
  engine: shell
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `speech: engine "shell" is not supported`) {
		t.Fatalf("LoadConfig error = %v", err)
	}
}

func TestLoadConfigKeychainTokenRef(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      keychain: goobers/acme-web
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := cfg.Repos[0].Token
	if got.Keychain != "goobers/acme-web" || got.sourceCount() != 1 {
		t.Fatalf("token = %+v, want one Keychain source", got)
	}
	if converted := got.CredentialTokenRef("repo"); converted.Keychain != got.Keychain {
		t.Fatalf("CredentialTokenRef = %+v, want Keychain service preserved", converted)
	}
}

// TestLoadConfigGitHubAppAuth: appId/installationId accept both the YAML
// number and string spellings GitHub surfaces (numeric IDs vs client-ID
// strings), normalized to strings; the loaded repo reports GitHubAppAuth.
func TestLoadConfigGitHubAppAuth(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    auth:
      kind: github-app
      appId: 123456
      installationId: "42"
      privateKey:
        file: /run/secrets/goobers-app.pem
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := cfg.Repos[0]
	if !repo.GitHubAppAuth() {
		t.Fatalf("GitHubAppAuth() = false for %+v", repo.Auth)
	}
	if repo.Auth.AppID != "123456" {
		t.Fatalf("appId = %q, want numeric YAML normalized to \"123456\"", repo.Auth.AppID)
	}
	if repo.Auth.InstallationID != "42" {
		t.Fatalf("installationId = %q, want \"42\"", repo.Auth.InstallationID)
	}
	if repo.Auth.PrivateKey == nil || repo.Auth.PrivateKey.File != "/run/secrets/goobers-app.pem" {
		t.Fatalf("privateKey = %+v, want file ref", repo.Auth.PrivateKey)
	}
}

func TestLoadConfigWorkcopies(t *testing.T) {
	base := `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
`
	cfg, err := LoadConfig(writeInstanceYAML(t, base))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PartialCloneEnabled() {
		t.Fatal("workcopies.partialClone must default to false")
	}
	if cfg.ObjectCacheEnabled() {
		t.Fatal("workcopies.objectCache must default to false")
	}

	cfg, err = LoadConfig(writeInstanceYAML(t, base+`
workcopies:
  partialClone: true
`))
	if err != nil {
		t.Fatalf("LoadConfig with workcopies: %v", err)
	}
	if !cfg.PartialCloneEnabled() {
		t.Fatal("workcopies.partialClone: true was not honored")
	}
	if cfg.ObjectCacheEnabled() {
		t.Fatal("workcopies.objectCache must stay false when only partialClone is set")
	}

	cfg, err = LoadConfig(writeInstanceYAML(t, base+`
workcopies:
  objectCache: true
`))
	if err != nil {
		t.Fatalf("LoadConfig with workcopies.objectCache: %v", err)
	}
	if !cfg.ObjectCacheEnabled() {
		t.Fatal("workcopies.objectCache: true was not honored")
	}
	if cfg.PartialCloneEnabled() {
		t.Fatal("workcopies.partialClone must stay false when only objectCache is set")
	}

	root := filepath.Join(t.TempDir(), "short")
	cfg, err = LoadConfig(writeInstanceYAML(t, base+fmt.Sprintf(`
workcopies:
  root: %q
`, root)))
	if err != nil {
		t.Fatalf("LoadConfig with workcopies root: %v", err)
	}
	layout, err := EffectiveWorkcopiesLayout(NewLayout("/instance").ForGaggle("builders"), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := layout.WorkcopiesDir(), filepath.Join(root, "builders"); got != want {
		t.Fatalf("WorkcopiesDir() = %q, want %q", got, want)
	}

	cfg.Workcopies.Root = "relative"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("Validate() error = %v, want absolute-path error", err)
	}
}

func TestEffectiveWorkcopiesLayoutGaggleOverrideWins(t *testing.T) {
	instanceRoot := filepath.Join(t.TempDir(), "instance-work")
	gaggleRoot := filepath.Join(t.TempDir(), "gaggle-work")
	cfg := &Config{Workcopies: &WorkcopiesConfig{Root: instanceRoot}}
	gaggle := &apiv1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: "builders"},
		Spec: apiv1.GaggleSpec{
			Workcopies: &apiv1.GaggleWorkcopies{Root: gaggleRoot},
		},
	}

	layout, err := EffectiveWorkcopiesLayout(NewLayout("/instance").ForGaggle("builders"), cfg, gaggle)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := layout.WorkcopiesDir(), filepath.Join(gaggleRoot, "builders"); got != want {
		t.Fatalf("WorkcopiesDir() = %q, want %q", got, want)
	}
	if got, want := layout.WorkcopiesBaseDir(), filepath.Join(gaggleRoot, "builders"); got != want {
		t.Fatalf("WorkcopiesBaseDir() = %q, want %q", got, want)
	}
	other, err := EffectiveWorkcopiesLayout(NewLayout("/instance").ForGaggle("reviewers"), cfg, &apiv1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewers"},
		Spec: apiv1.GaggleSpec{
			Workcopies: &apiv1.GaggleWorkcopies{Root: gaggleRoot},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if layout.WorkcopiesDir() == other.WorkcopiesDir() {
		t.Fatalf("gaggles share workcopies directory %q", layout.WorkcopiesDir())
	}
}

func TestLargeRepoPresetResolvesDefaultsAndOverrides(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: monolith
    token:
      env: GITHUB_TOKEN
    largeRepo: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := cfg.Repos[0]
	if !repo.Pinned() || repo.Workspace == nil || repo.Workspace.CleanPolicy != "" {
		t.Fatalf("workspace = %+v, want pinned with none clean-policy default", repo.Workspace)
	}
	if repo.PathLength == nil || repo.PathLength.Disabled {
		t.Fatalf("pathLength = %+v, want enabled preset default", repo.PathLength)
	}
	if repo.DefaultStageTimeout != LargeRepoDefaultStageTimeout {
		t.Fatalf("defaultStageTimeout = %q, want %q", repo.DefaultStageTimeout, LargeRepoDefaultStageTimeout)
	}
	if repo.RunControls == nil ||
		repo.RunControls.StalledRunTimeout != LargeRepoStalledRunTimeout ||
		repo.RunControls.MaxRunDuration != LargeRepoMaxRunDuration {
		t.Fatalf("runControls = %+v, want large-repo defaults", repo.RunControls)
	}
	if got := repo.EffectiveRunControls((RunConditions{MaxRepasses: 2, StalledRunTimeout: "30m"}).RunControls()); got.MaxRepasses != 2 ||
		got.StalledRunTimeout != LargeRepoStalledRunTimeout || got.MaxRunDuration != LargeRepoMaxRunDuration {
		t.Fatalf("EffectiveRunControls = %+v, want preset over instance defaults", got)
	}

	path = writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: monolith
    token:
      env: GITHUB_TOKEN
    largeRepo: true
    workspace:
      pinned: false
    pathLength:
      disabled: true
    defaultStageTimeout: 45m
    runControls:
      stalledRunTimeout: 90m
      maxRunDuration: 8h
`)
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with overrides: %v", err)
	}
	repo = cfg.Repos[0]
	if repo.Pinned() {
		t.Fatalf("workspace = %+v, want explicit pinned:false relaxation", repo.Workspace)
	}
	if repo.PathLength == nil || !repo.PathLength.Disabled {
		t.Fatalf("pathLength = %+v, want explicit disabled relaxation", repo.PathLength)
	}
	if repo.DefaultStageTimeout != "45m" {
		t.Fatalf("defaultStageTimeout = %q, want tightened 45m", repo.DefaultStageTimeout)
	}
	if repo.RunControls.StalledRunTimeout != "90m" || repo.RunControls.MaxRunDuration != "8h" {
		t.Fatalf("runControls = %+v, want explicit overrides", repo.RunControls)
	}
	if got := repo.EffectiveDefaultStageTimeout("20m"); got != "45m" {
		t.Fatalf("EffectiveDefaultStageTimeout = %q, want explicit 45m", got)
	}
}

func TestLoadConfigRejectsUnknownWorkspaceField(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: monolith
    token:
      env: GITHUB_TOKEN
    largeRepo: true
    workspace:
      pinned: false
      pinend: true
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `unknown field "pinend"`) {
		t.Fatalf("LoadConfig error = %v, want unknown workspace field rejection", err)
	}
}

func TestLoadConfigRepoPathLength(t *testing.T) {
	cfg, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
    pathLength:
      maxPathLength: 320
      buildOutputAllowance: 48
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := cfg.Repos[0].PathLength
	if got == nil || got.MaxPathLength != 320 || got.BuildOutputAllowance != 48 || got.Disabled {
		t.Fatalf("pathLength = %+v", got)
	}

	cfg.Repos[0].PathLength.BuildOutputAllowance = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "pathLength.buildOutputAllowance must not be negative") {
		t.Fatalf("Validate error = %v, want negative allowance rejection", err)
	}
}

func TestEffectivePortalConfigAppliesDefaults(t *testing.T) {
	cfg := &Config{}
	got := cfg.EffectivePortalConfig()
	if got.Brand.Name != "goobers" || got.Brand.Tagline != "local operations" || got.Brand.ScopeMark != "G" {
		t.Fatalf("EffectivePortalConfig() = %+v", got.Brand)
	}

	custom := (&Config{Portal: PortalConfig{Brand: PortalBrandConfig{Name: "acme ops"}}}).EffectivePortalConfig()
	if custom.Brand.ScopeMark != "A" {
		t.Fatalf("scopeMark = %q, want A", custom.Brand.ScopeMark)
	}
}

func TestPortalConfigValidate(t *testing.T) {
	valid := PortalConfig{
		Brand: PortalBrandConfig{
			Name:       "Acme Ops",
			Tagline:    "AI workforce platform",
			LogoURL:    "/assets/logo.svg",
			FaviconURL: "/assets/favicon.ico",
		},
		Theme: PortalThemeConfig{
			AccentLight:    "#6847d9",
			AccentDark:     "rgb(12 34 56)",
			AccentInkLight: "rebeccapurple",
		},
		Support: PortalSupportConfig{
			DocsURL:   "https://acme.example/docs",
			IssuesURL: "https://acme.example/help",
			ChatURL:   "slack://channel/C000EXAMPLE",
			Links: []PortalSupportLink{{
				Label: "Runbooks",
				URL:   "https://acme.example/runbooks",
			}},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() valid config error = %v", err)
	}

	tests := []struct {
		name    string
		config  PortalConfig
		wantErr string
	}{
		{
			name:    "name too long",
			config:  PortalConfig{Brand: PortalBrandConfig{Name: strings.Repeat("a", 65)}},
			wantErr: "brand.name",
		},
		{
			name:    "logo must stay local",
			config:  PortalConfig{Brand: PortalBrandConfig{LogoURL: "https://example.com/logo.svg"}},
			wantErr: "brand.logoUrl",
		},
		{
			name:    "theme blocks injection",
			config:  PortalConfig{Theme: PortalThemeConfig{AccentLight: "red;display:none"}},
			wantErr: "theme.accentLight",
		},
		{
			name:    "docs must be https",
			config:  PortalConfig{Support: PortalSupportConfig{DocsURL: "http://example.com/docs"}},
			wantErr: "support.docsUrl",
		},
		{
			name:    "chat scheme limited",
			config:  PortalConfig{Support: PortalSupportConfig{ChatURL: "mailto:help@example.com"}},
			wantErr: "support.chatUrl",
		},
		{
			name: "links limited",
			config: PortalConfig{Support: PortalSupportConfig{Links: []PortalSupportLink{
				{Label: "1", URL: "https://example.com/1"},
				{Label: "2", URL: "https://example.com/2"},
				{Label: "3", URL: "https://example.com/3"},
				{Label: "4", URL: "https://example.com/4"},
				{Label: "5", URL: "https://example.com/5"},
				{Label: "6", URL: "https://example.com/6"},
				{Label: "7", URL: "https://example.com/7"},
			}}},
			wantErr: "support.links must contain 6 entries or fewer",
		},
		{
			name: "link label required",
			config: PortalConfig{Support: PortalSupportConfig{Links: []PortalSupportLink{{
				Label: " ",
				URL:   "https://example.com/runbooks",
			}}}},
			wantErr: "support.links[0].label",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigExternalTelemetryConnectorReferencesCredentials(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
externalTelemetry:
  connectors:
    - name: production-metrics
      kind: adx-kql-rest
      version: v1
      auth:
        mode: bearer-token
        token:
          env: ADX_TOKEN
      policy:
        timeout: 20s
        maxAttempts: 2
        retryBackoff: 1s
        maxRows: 100
        maxBytes: 65536
      network:
        allowedHosts:
          - metrics.kusto.windows.net
      config:
        cluster: https://metrics.kusto.windows.net
        database: production
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.ExternalTelemetry.Connectors) != 1 {
		t.Fatalf("connectors = %+v", cfg.ExternalTelemetry.Connectors)
	}
	connector := cfg.ExternalTelemetry.Connectors[0]
	if connector.Name != "production-metrics" || connector.Auth.Token == nil ||
		connector.Auth.Token.Env != "ADX_TOKEN" || strings.Contains(string(connector.Config), "ADX_TOKEN") {
		t.Fatalf("connector = %+v", connector)
	}
}

func TestConfigExternalTelemetryRejectsStageExposedTokenEnv(t *testing.T) {
	tests := []struct {
		name           string
		tokenEnv       string
		envPassthrough []string
	}{
		{name: "explicit passthrough", tokenEnv: "ADX_TOKEN", envPassthrough: []string{"ADX_TOKEN"}},
		{name: "built-in allowlist", tokenEnv: "HOME"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{
				Runner: RunnerConfig{EnvPassthrough: test.envPassthrough},
				ExternalTelemetry: externaltelemetry.Configuration{
					Connectors: []externaltelemetry.ConnectorConfig{{
						Name:    "metrics",
						Kind:    "adx-kql-rest",
						Version: "v1",
						Auth: externaltelemetry.AuthConfig{
							Mode:  externaltelemetry.AuthBearerToken,
							Token: &externaltelemetry.CredentialRef{Env: test.tokenEnv},
						},
						Config: json.RawMessage(`{}`),
					}},
				},
			}
			if err := cfg.Validate(); err == nil ||
				!strings.Contains(err.Error(), "externalTelemetry.connectors[0]") ||
				!strings.Contains(err.Error(), "must not be exposed to stages") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestLoadConfigExternalTelemetryRejectsInlineCredentialValue(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
externalTelemetry:
  connectors:
    - name: metrics
      kind: adx-kql-rest
      version: v1
      auth:
        mode: bearer-token
        token:
          value: not-allowed
      config:
        cluster: https://metrics.kusto.windows.net
        database: production
`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), `unknown field "value"`) {
		t.Fatalf("LoadConfig error = %v", err)
	}
}

func TestLoadConfigValidatesExternalTelemetryWithoutTelemetryRetention(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
externalTelemetry:
  connectors:
    - name: Invalid_Name
      kind: fake
      version: v1
      config:
        source: fixture
`)
	if _, err := LoadConfig(path); err == nil ||
		!strings.Contains(err.Error(), "externalTelemetry") ||
		!strings.Contains(err.Error(), "must match") {
		t.Fatalf("LoadConfig error = %v", err)
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
		// The github-app auth block (#3274) mirrors repos[]' validation: kind
		// github-app forbids a sibling token ref, requires its identity
		// fields, and is meaningless anywhere but a remote git url.
		{
			name: "github-app auth with sibling token ref",
			source: WorkflowSource{
				Kind:  "git",
				URL:   "https://example.com/config.git",
				Token: &TokenRef{Env: "CONFIG_TOKEN"},
				Auth:  workflowSourceTestAppAuth(),
			},
			wantErr: "must not configure token.env, token.file, token.keychain, or token.store — the installation token is minted",
		},
		{
			name: "auth kind pat is not how a static credential is spelled",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{Kind: GitHubAuthPAT},
			},
			wantErr: "unsupported auth kind",
		},
		{
			name: "github-app auth missing appId",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{
					Kind:           GitHubAuthApp,
					InstallationID: "10000001",
					PrivateKey:     &TokenRef{File: "/run/secrets/app-key.pem"},
				},
			},
			wantErr: "auth.appId is required",
		},
		{
			name: "github-app auth missing installationId",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{
					Kind:       GitHubAuthApp,
					AppID:      "123456",
					PrivateKey: &TokenRef{File: "/run/secrets/app-key.pem"},
				},
			},
			wantErr: "auth.installationId is required",
		},
		{
			name: "github-app auth non-numeric installationId",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{
					Kind:           GitHubAuthApp,
					AppID:          "123456",
					InstallationID: "Iv1NOTNUMERIC",
					PrivateKey:     &TokenRef{File: "/run/secrets/app-key.pem"},
				},
			},
			wantErr: "must be the numeric installation ID",
		},
		{
			name: "github-app auth missing privateKey",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{
					Kind:           GitHubAuthApp,
					AppID:          "123456",
					InstallationID: "10000001",
				},
			},
			wantErr: "auth.privateKey must reference exactly one",
		},
		{
			name: "github-app auth privateKey with two sources",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{
					Kind:           GitHubAuthApp,
					AppID:          "123456",
					InstallationID: "10000001",
					PrivateKey:     &TokenRef{Env: "APP_KEY", File: "/run/secrets/app-key.pem"},
				},
			},
			wantErr: "auth.privateKey must reference exactly one",
		},
		{
			name: "github-app auth with ADO fields",
			source: WorkflowSource{
				Kind: "git",
				URL:  "https://example.com/config.git",
				Auth: &RepoAuthConfig{
					Kind:           GitHubAuthApp,
					Tenant:         "contoso",
					AppID:          "123456",
					InstallationID: "10000001",
					PrivateKey:     &TokenRef{File: "/run/secrets/app-key.pem"},
				},
			},
			wantErr: "auth.tenant and auth.clientId",
		},
		{
			name: "local git with auth",
			source: WorkflowSource{
				Kind: "git",
				Path: "config",
				Auth: workflowSourceTestAppAuth(),
			},
			wantErr: "auth is only valid for a remote git url",
		},
		{
			name: "local directory with auth",
			source: WorkflowSource{
				Kind: "local-dir",
				Path: "config",
				Auth: workflowSourceTestAppAuth(),
			},
			wantErr: "accepts only path",
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

// workflowSourceTestAppAuth is a structurally complete github-app auth block
// (#3274) for tests that reject it on grounds other than its own fields.
func workflowSourceTestAppAuth() *RepoAuthConfig {
	return &RepoAuthConfig{
		Kind:           GitHubAuthApp,
		AppID:          "123456",
		InstallationID: "10000001",
		PrivateKey:     &TokenRef{File: "/run/secrets/app-key.pem"},
	}
}

// TestWorkflowSourceGitHubAppAuthValidates pins #3274's accepted shape: a
// remote git source whose only identity mechanism is the github-app auth
// block, with no token ref at all.
func TestWorkflowSourceGitHubAppAuthValidates(t *testing.T) {
	cfg := Config{WorkflowSource: &WorkflowSource{
		Kind: WorkflowSourceKindGit,
		URL:  "https://github.com/example-org/example-config",
		Ref:  "main",
		Auth: workflowSourceTestAppAuth(),
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a github-app workflowSource", err)
	}
	if !cfg.WorkflowSource.GitHubAppAuth() {
		t.Fatal("GitHubAppAuth() = false, want true")
	}
}

// TestConfigRejectsWorkflowSourceAppKeyExposedToStages extends the existing
// workflowSource.token.env guard to the App private key (#3274): the key can
// mint tokens for every repository the installation covers, so an env name
// stages can also see is refused at load, mirroring repos[] and daemonIdentity.
func TestConfigRejectsWorkflowSourceAppKeyExposedToStages(t *testing.T) {
	cfg := Config{
		WorkflowSource: &WorkflowSource{
			Kind: WorkflowSourceKindGit,
			URL:  "https://example.com/config.git",
			Auth: &RepoAuthConfig{
				Kind:           GitHubAuthApp,
				AppID:          "123456",
				InstallationID: "10000001",
				PrivateKey:     &TokenRef{Env: "WORKFLOW_SOURCE_APP_KEY"},
			},
		},
		Runner: RunnerConfig{EnvPassthrough: []string{"WORKFLOW_SOURCE_APP_KEY"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "workflowSource.auth.privateKey.env") ||
		!strings.Contains(err.Error(), "must not be exposed to stages") {
		t.Fatalf("Validate error = %v, want workflow-source App key exposure rejection", err)
	}
}

// TestConfigRejectsWorkflowSourceStoreBackedAppKeyWithoutStores pins the #683
// fail-closed rule for the App key ref (#3274): a store-backed privateKey with
// no declared secretStores is a load-time error, not a first-mint surprise.
func TestConfigRejectsWorkflowSourceStoreBackedAppKeyWithoutStores(t *testing.T) {
	cfg := Config{WorkflowSource: &WorkflowSource{
		Kind: WorkflowSourceKindGit,
		URL:  "https://example.com/config.git",
		Auth: &RepoAuthConfig{
			Kind:           GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "10000001",
			PrivateKey:     &TokenRef{Store: "prod-kv/app-key"},
		},
	}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "workflowSource.auth.privateKey") {
		t.Fatalf("Validate error = %v, want store-ref-without-stores rejection", err)
	}
}

// TestLoadConfigAcceptsWorkflowSourceGitHubAppFixture loads #3274's acceptance
// fixture — a sanitized copy of the cloud deployment's real instance.yaml,
// combining per-repo App auth and workflowSource App auth in one document —
// through the full LoadConfig strict-decode + Validate path. Its daemonIdentity
// is a PAT rather than an App: the fixture's repos span two owners, and #3414
// rejects a single-installation App identity in that shape because it cannot
// mint for both. Before WorkflowSource carried Auth, the strict decoder refused the
// document outright ("unknown field").
func TestLoadConfigAcceptsWorkflowSourceGitHubAppFixture(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "api", "schemas", "testdata", "instance-workflowsource-app-auth.fixture.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.WorkflowSource == nil || !cfg.WorkflowSource.GitHubAppAuth() {
		t.Fatalf("WorkflowSource = %+v, want github-app auth", cfg.WorkflowSource)
	}
	if got := string(cfg.WorkflowSource.Auth.InstallationID); got != "10000001" {
		t.Fatalf("workflowSource installation = %q, want the fixture's org installation", got)
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

// TestRetentionConfigEnabledWithNoLimitsIsRejected covers the exact silent
// no-op combination from #2052: enabling retention without setting either
// limit previously pruned nothing, giving an operator false confidence that
// disk usage was bounded.
func TestRetentionConfigEnabledWithNoLimitsIsRejected(t *testing.T) {
	if err := (&Config{Retention: RetentionConfig{Enabled: true}}).Validate(); err == nil || !strings.Contains(err.Error(), "retention.enabled requires at least one") {
		t.Fatalf("Validate(enabled, no limits) error = %v, want a retention.enabled-requires-a-limit error", err)
	}

	// A single configured axis remains a valid, intentional configuration —
	// this must NOT be rejected by the new check.
	if err := (&Config{Retention: RetentionConfig{Enabled: true, MaxRetainedWorktreeBytes: 1}}).Validate(); err != nil {
		t.Fatalf("Validate(enabled, byte cap only) error = %v, want nil", err)
	}
	if err := (&Config{Retention: RetentionConfig{Enabled: true, RetainedWorktreeMaxAge: "1h"}}).Validate(); err != nil {
		t.Fatalf("Validate(enabled, age limit only) error = %v, want nil", err)
	}

	// Disabled retention with no limits is unaffected — it already prunes
	// nothing by design, so there is no silent-no-op trap to guard against.
	if err := (&Config{Retention: RetentionConfig{}}).Validate(); err != nil {
		t.Fatalf("Validate(disabled, no limits) error = %v, want nil", err)
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

func TestMaxRunDuration(t *testing.T) {
	if got, err := (RunConditions{}).MaxRunDurationDuration(); err != nil || got != 0 {
		t.Fatalf("default MaxRunDurationDuration = %s, %v; want disabled", got, err)
	}
	if got, err := (RunConditions{MaxRunDuration: "6h"}).MaxRunDurationDuration(); err != nil || got != 6*time.Hour {
		t.Fatalf("MaxRunDurationDuration = %s, %v; want 6h", got, err)
	}
	for _, value := range []string{"not-a-duration", "0s", "-1m"} {
		t.Run(value, func(t *testing.T) {
			cfg := Config{RunConditions: RunConditions{MaxRunDuration: value}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "maxRunDuration") {
				t.Fatalf("Validate() error = %v, want maxRunDuration error", err)
			}
		})
	}
}

func TestRunConditionsExposeRunControlDefaults(t *testing.T) {
	conditions := RunConditions{MaxRepasses: 6, StalledRunTimeout: "3h", MaxRunDuration: "8h"}
	if got := conditions.RunControls(); got.MaxRepasses != 6 || got.StalledRunTimeout != "3h" || got.MaxRunDuration != "8h" {
		t.Fatalf("RunControls = %+v", got)
	}
	if err := (&Config{RunConditions: RunConditions{MaxRepasses: -1}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "maxRepasses") {
		t.Fatalf("negative maxRepasses error = %v", err)
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

// TestLoadConfigRejectsInsecureNonLoopbackOTLP reproduces #3333's incident
// shape verbatim (telemetry.otlp.insecure: true against a non-loopback
// collector host:port) and pins the refusal message naming both escape
// routes — a loopback sidecar collector, or a TLS endpoint — so the boot
// failure teaches the fix instead of just naming the rule.
func TestLoadConfigRejectsInsecureNonLoopbackOTLP(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
telemetry:
  otlp:
    endpoint: goobers-collector.goobers-system:4317
    insecure: true
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig: want error for insecure non-loopback OTLP endpoint, got nil")
	}
	for _, want := range []string{
		"insecure mode is allowed only for localhost or a loopback IP",
		"loopback sidecar collector",
		"TLS collector",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("LoadConfig error = %q, want it to contain %q", err.Error(), want)
		}
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

func TestLoadConfigEngineEnvironmentOverridesFile(t *testing.T) {
	t.Setenv(TemporalHostPortEnv, "temporal.internal:7233")
	t.Setenv(TemporalNamespaceEnv, "production")
	t.Setenv(TaskQueueEnv, "production-engine")
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
engine:
  hostPort: localhost:7233
  namespace: development
  taskQueue: development-engine
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := EngineConfig{HostPort: "temporal.internal:7233", Namespace: "production", TaskQueue: "production-engine"}
	if got := cfg.EffectiveEngineConfig(); got != want {
		t.Fatalf("EffectiveEngineConfig = %+v, want %+v", got, want)
	}
}

func TestResolveEngineConfig(t *testing.T) {
	t.Run("defaults without enabling projection", func(t *testing.T) {
		resolved, configured, err := (&Config{}).ResolveEngineConfig(func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		if configured {
			t.Fatal("unconfigured engine unexpectedly enabled")
		}
		if resolved != (EngineConfig{HostPort: DefaultTemporalHostPort, Namespace: DefaultTemporalNamespace, TaskQueue: DefaultEngineTaskQueue}) {
			t.Fatalf("resolved engine = %+v", resolved)
		}
	})

	t.Run("invalid YAML is actionable", func(t *testing.T) {
		cfg := Config{Engine: &EngineConfig{HostPort: "missing-port"}}
		_, _, err := cfg.ResolveEngineConfig(func(string) (string, bool) { return "", false })
		if err == nil || !strings.Contains(err.Error(), `engine: hostPort "missing-port" must be in host:port form`) {
			t.Fatalf("ResolveEngineConfig error = %v", err)
		}
	})

	t.Run("empty environment override fails closed", func(t *testing.T) {
		_, _, err := (&Config{}).ResolveEngineConfig(func(key string) (string, bool) {
			return "", key == TemporalNamespaceEnv
		})
		if err == nil || !strings.Contains(err.Error(), TemporalNamespaceEnv+" must not be empty") {
			t.Fatalf("ResolveEngineConfig error = %v", err)
		}
	})

	t.Run("compatibility aliases retain precedence", func(t *testing.T) {
		env := map[string]string{
			TemporalHostPortEnv:        "canonical:7233",
			TemporalAddressEnv:         "goobers-alias:7233",
			TemporalAddressLegacyEnv:   "legacy:7233",
			TemporalNamespaceEnv:       "canonical-namespace",
			TemporalNamespaceLegacyEnv: "legacy-namespace",
			TaskQueueEnv:               "canonical-queue",
			TemporalTaskQueueEnv:       "goobers-alias-queue",
			TemporalTaskQueueLegacyEnv: "legacy-queue",
		}
		resolved, _, err := (&Config{}).ResolveEngineConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		})
		if err != nil {
			t.Fatal(err)
		}
		want := EngineConfig{HostPort: "canonical:7233", Namespace: "canonical-namespace", TaskQueue: "canonical-queue"}
		if resolved != want {
			t.Fatalf("resolved engine = %+v, want %+v", resolved, want)
		}
	})

	t.Run("empty compatibility alias falls through", func(t *testing.T) {
		env := map[string]string{
			TemporalAddressEnv:       "",
			TemporalAddressLegacyEnv: "legacy:7233",
		}
		resolved, _, err := (&Config{}).ResolveEngineConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.HostPort != "legacy:7233" {
			t.Fatalf("hostPort = %q", resolved.HostPort)
		}
	})

	for name, env := range map[string]map[string]string{
		"goobers address":    {TemporalAddressEnv: "temporal:7233"},
		"legacy address":     {TemporalAddressLegacyEnv: "temporal:7233"},
		"legacy namespace":   {TemporalNamespaceLegacyEnv: "production"},
		"goobers task queue": {TemporalTaskQueueEnv: "production"},
		"legacy task queue":  {TemporalTaskQueueLegacyEnv: "production"},
	} {
		t.Run(name, func(t *testing.T) {
			resolved, _, err := (&Config{}).ResolveEngineConfig(func(key string) (string, bool) {
				value, ok := env[key]
				return value, ok
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(name, "address") && resolved.HostPort != "temporal:7233" {
				t.Fatalf("hostPort = %q", resolved.HostPort)
			}
			if strings.Contains(name, "namespace") && resolved.Namespace != "production" {
				t.Fatalf("namespace = %q", resolved.Namespace)
			}
			if strings.Contains(name, "task queue") && resolved.TaskQueue != "production" {
				t.Fatalf("taskQueue = %q", resolved.TaskQueue)
			}
		})
	}
}

func TestEngineProjectionActivationIgnoresNamespaceAndTaskQueueOverrides(t *testing.T) {
	cfg := &Config{}
	resolved, env, err := cfg.resolveEngineConfig(func(key string) (string, bool) {
		values := map[string]string{
			TemporalNamespaceEnv: "production",
			TaskQueueEnv:         "production",
		}
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Engine = &resolved
	cfg.engineResolutionApplied = true
	cfg.engineProjectionEnabled = env.hostOverride

	if cfg.EngineProjectionEnabled() {
		t.Fatal("namespace/task-queue-only overrides enabled projection")
	}
	if got := cfg.EffectiveEngineConfig(); got.Namespace != "production" || got.TaskQueue != "production" {
		t.Fatalf("effective engine config = %+v", got)
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

// TestLoadConfigCredentialsBlock covers first-party capability and BYO MCP
// credential grants.
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
  - mcp: sharepoint
    token:
      env: SHAREPOINT_MCP_TOKEN
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Credentials) != 3 {
		t.Fatalf("expected 3 credentials, got %+v", cfg.Credentials)
	}
	if cfg.Credentials[0].Capability != "agent:model" || cfg.Credentials[0].Token.Env != "COPILOT_GITHUB_TOKEN" {
		t.Fatalf("unexpected credentials[0]: %+v", cfg.Credentials[0])
	}
	if cfg.Credentials[1].Capability != "repo:push" || cfg.Credentials[1].Token.File != "/run/secrets/push-token" {
		t.Fatalf("unexpected credentials[1]: %+v", cfg.Credentials[1])
	}
	if cfg.Credentials[2].MCP != "sharepoint" || cfg.Credentials[2].Token.Env != "SHAREPOINT_MCP_TOKEN" {
		t.Fatalf("unexpected credentials[2]: %+v", cfg.Credentials[2])
	}
}

func TestLoadConfigRejectsMalformedBYOMCPCredentialGrant(t *testing.T) {
	for name, grant := range map[string]string{
		"missing selector": `token:
      env: MCP_TOKEN`,
		"both selectors": `capability: agent:model
    mcp: sharepoint
    token:
      env: MCP_TOKEN`,
		"invalid name": `mcp: SharePoint
    token:
      env: MCP_TOKEN`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
credentials:
  - `+grant+`
`)
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("malformed BYO MCP credential grant passed validation")
			}
		})
	}
}

func TestConfigCapabilityCredentialEnvExposure(t *testing.T) {
	tests := []struct {
		name           string
		tokenEnv       string
		envPassthrough []string
		wantErr        bool
	}{
		{name: "explicit passthrough", tokenEnv: "MODEL_TOKEN", envPassthrough: []string{"MODEL_TOKEN"}, wantErr: true},
		{name: "built-in allowlist", tokenEnv: "HOME", wantErr: true},
		{name: "not stage exposed", tokenEnv: "MODEL_TOKEN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{
				Runner: RunnerConfig{EnvPassthrough: test.envPassthrough},
				Credentials: []CredentialGrant{{
					Capability: "agent:model",
					Token:      TokenRef{Env: test.tokenEnv},
				}},
			}
			err := cfg.Validate()
			if !test.wantErr {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil ||
				!strings.Contains(err.Error(), `credentials[0] (capability "agent:model")`) ||
				!strings.Contains(err.Error(), "must not be exposed to stages") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestConfigRejectsStageExposedBYOMCPCredentialEnv(t *testing.T) {
	tests := []struct {
		name           string
		tokenEnv       string
		envPassthrough []string
	}{
		{name: "explicit passthrough", tokenEnv: "SHAREPOINT_MCP_TOKEN", envPassthrough: []string{"SHAREPOINT_MCP_TOKEN"}},
		{name: "built-in allowlist", tokenEnv: "HOME"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{
				Runner: RunnerConfig{EnvPassthrough: test.envPassthrough},
				Credentials: []CredentialGrant{{
					MCP:   "sharepoint",
					Token: TokenRef{Env: test.tokenEnv},
				}},
			}
			err := cfg.Validate()
			if err == nil ||
				!strings.Contains(err.Error(), `credentials[0] (MCP credential "sharepoint")`) ||
				!strings.Contains(err.Error(), "must not be exposed to stages") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
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

func TestConfigValidatePinnedWorkspace(t *testing.T) {
	base := RepoRef{
		Provider: "github", Owner: "acme", Name: "large",
		Token: TokenRef{Env: "GITHUB_TOKEN"},
	}
	valid := base
	valid.Workspace = &RepoWorkspaceConfig{Pinned: true}
	if err := (&Config{Repos: []RepoRef{valid}}).Validate(); err != nil {
		t.Fatalf("valid pinned workspace: %v", err)
	}

	contradictory := base
	contradictory.Workspace = &RepoWorkspaceConfig{Pinned: true, Worktrees: true}
	if err := (&Config{Repos: []RepoRef{contradictory}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "VER:") || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("contradictory workspace error = %v", err)
	}

	badPolicy := base
	badPolicy.Workspace = &RepoWorkspaceConfig{Pinned: true, CleanPolicy: "sometimes"}
	if err := (&Config{Repos: []RepoRef{badPolicy}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "cleanPolicy") {
		t.Fatalf("invalid clean policy error = %v", err)
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
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Auth: &RepoAuthConfig{Kind: ADOAuthAzureCLI}},
			}},
		},
		{
			name: "ado missing project",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{Kind: ADOAuthAzureCLI}},
			}},
			wantErr: "project is required",
		},
		{
			name: "ado identity auth rejects PAT",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Token: TokenRef{Env: "T"}, Auth: &RepoAuthConfig{Kind: ADOAuthWorkloadIdentity}},
			}},
			wantErr: "must not configure token",
		},
		{
			name: "valid gitea",
			cfg: Config{Repos: []RepoRef{
				{Provider: "gitea", BaseURL: "https://gitea.example.com", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}},
			}},
		},
		{
			name: "gitea missing baseUrl",
			cfg: Config{Repos: []RepoRef{
				{Provider: "gitea", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}},
			}},
			wantErr: "baseUrl is required",
		},
		{
			name: "gitea rejects project",
			cfg: Config{Repos: []RepoRef{
				{Provider: "gitea", BaseURL: "https://gitea.example.com", Owner: "acme", Project: "widgets", Name: "web", Token: TokenRef{Env: "T"}},
			}},
			wantErr: "project is only valid for provider",
		},
		{
			name: "gitea rejects auth block",
			cfg: Config{Repos: []RepoRef{
				{Provider: "gitea", BaseURL: "https://gitea.example.com", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}, Auth: &RepoAuthConfig{Kind: ADOAuthAzureCLI}},
			}},
			wantErr: "supports only a static token",
		},
		{
			name: "gitea missing token",
			cfg: Config{Repos: []RepoRef{
				{Provider: "gitea", BaseURL: "https://gitea.example.com", Owner: "acme", Name: "web"},
			}},
			wantErr: "gitea auth requires token",
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
			wantErr: "exactly one of env, file, keychain, or store",
		},
		{
			name: "both env and file",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T", File: "/f"}},
			}},
			wantErr: "exactly one of env, file, keychain, or store",
		},
		{
			name: "valid",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}},
			}},
		},
		{
			name: "valid github-app",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
					PrivateKey: &TokenRef{File: "/run/secrets/app.pem"},
				}},
			}},
		},
		{
			name: "valid github-app store-backed key",
			cfg: Config{
				SecretStores: []SecretStoreConfig{{
					Name: "prod-kv", Kind: SecretStoreKindAzureKeyVault,
					VaultURI: "https://acme.vault.azure.net",
					Auth:     &SecretStoreAuthConfig{Kind: SecretStoreAuthWorkloadIdentity},
				}},
				Repos: []RepoRef{
					{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
						Kind: GitHubAuthApp, AppID: "Iv1.abcdef", InstallationID: "42",
						PrivateKey: &TokenRef{Store: "prod-kv/app-key"},
					}},
				},
			},
		},
		{
			name: "github-app rejects token alongside minting",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}, Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
					PrivateKey: &TokenRef{File: "/run/secrets/app.pem"},
				}},
			}},
			wantErr: "must not configure token",
		},
		{
			name: "github-app missing appId",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, InstallationID: "42",
					PrivateKey: &TokenRef{File: "/run/secrets/app.pem"},
				}},
			}},
			wantErr: "auth.appId is required",
		},
		{
			name: "github-app missing installationId",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456",
					PrivateKey: &TokenRef{File: "/run/secrets/app.pem"},
				}},
			}},
			wantErr: "auth.installationId is required",
		},
		{
			name: "github-app non-numeric installationId",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "acme-corp",
					PrivateKey: &TokenRef{File: "/run/secrets/app.pem"},
				}},
			}},
			wantErr: "must be the numeric installation ID",
		},
		{
			name: "github-app missing privateKey",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
				}},
			}},
			wantErr: "auth.privateKey must reference exactly one",
		},
		{
			name: "github-app privateKey with two sources",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
					PrivateKey: &TokenRef{Env: "K", File: "/run/secrets/app.pem"},
				}},
			}},
			wantErr: "auth.privateKey must reference exactly one",
		},
		{
			name: "github-app privateKey undeclared store",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
					PrivateKey: &TokenRef{Store: "missing-kv/app-key"},
				}},
			}},
			wantErr: "not declared under secretStores",
		},
		{
			name: "github-app rejects ADO fields",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
					Kind: GitHubAuthApp, Tenant: "contoso", AppID: "123456", InstallationID: "42",
					PrivateKey: &TokenRef{File: "/run/secrets/app.pem"},
				}},
			}},
			wantErr: "only valid for ADO auth kinds",
		},
		{
			name: "github pat kind rejects app fields",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}, Auth: &RepoAuthConfig{
					Kind: GitHubAuthPAT, AppID: "123456",
				}},
			}},
			wantErr: "only valid for auth kind \"github-app\"",
		},
		{
			name: "github unsupported auth kind",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{Kind: "workload-identity"}},
			}},
			wantErr: "unsupported GitHub auth kind",
		},
		{
			name: "ado rejects github-app fields",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Auth: &RepoAuthConfig{
					Kind: ADOAuthAzureCLI, AppID: "123456",
				}},
			}},
			wantErr: "only valid for provider \"github\"",
		},
		{
			name: "valid repo policy",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}, Policy: &RepoPolicyExpectation{
					Branch: "main", RequiredMergeMethod: "squash", MergeQueueRequired: true,
					RequiredStatusChecks: []string{"make ci"},
				}},
			}},
		},
		{
			name: "policy rejected for ado repo (#916 V1 GitHub-only scope)",
			cfg: Config{Repos: []RepoRef{
				{Provider: "ado", Owner: "acme", Project: "widgets", Name: "web", Token: TokenRef{Env: "T"}, Policy: &RepoPolicyExpectation{
					RequiredMergeMethod: "squash",
				}},
			}},
			wantErr: "policy is only supported for provider \"github\"",
		},
		{
			name: "policy rejects invalid requiredMergeMethod",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}, Policy: &RepoPolicyExpectation{
					RequiredMergeMethod: "fast-forward",
				}},
			}},
			wantErr: "requiredMergeMethod must be",
		},
		{
			name: "policy rejects empty requiredStatusChecks entry",
			cfg: Config{Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "web", Token: TokenRef{Env: "T"}, Policy: &RepoPolicyExpectation{
					RequiredStatusChecks: []string{"make ci", "  "},
				}},
			}},
			wantErr: "requiredStatusChecks entries must not be empty",
		},
		{
			name: "github-app privateKey env exposed via passthrough",
			cfg: Config{
				Runner: RunnerConfig{EnvPassthrough: []string{"GOOBERS_APP_KEY"}},
				Repos: []RepoRef{
					{Provider: "github", Owner: "acme", Name: "web", Auth: &RepoAuthConfig{
						Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
						PrivateKey: &TokenRef{Env: "GOOBERS_APP_KEY"},
					}},
				},
			},
			wantErr: "must not be exposed to stages",
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
			wantErr: "exactly one of env, file, keychain, or store",
		},
		{
			name: "credentials both env and file",
			cfg: Config{Credentials: []CredentialGrant{
				{Capability: "agent:model", Token: TokenRef{Env: "T", File: "/f"}},
			}},
			wantErr: "exactly one of env, file, keychain, or store",
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
			name: "runner harness command override valid",
			cfg: Config{Runner: RunnerConfig{HarnessCommand: map[string][]string{
				"copilot":     {"agency", "copilot"},
				"claude-code": {"claude"},
			}}},
		},
		{
			name: "runner harness command unknown harness rejected",
			cfg: Config{Runner: RunnerConfig{HarnessCommand: map[string][]string{
				"agency": {"agency", "copilot"},
			}}},
			wantErr: "unknown harness",
		},
		{
			name: "runner harness command empty argv rejected",
			cfg: Config{Runner: RunnerConfig{HarnessCommand: map[string][]string{
				"copilot": {},
			}}},
			wantErr: "command must not be empty",
		},
		{
			name: "runner harness command empty program rejected",
			cfg: Config{Runner: RunnerConfig{HarnessCommand: map[string][]string{
				"copilot": {"   "},
			}}},
			wantErr: "program name",
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
			wantErr: "must reference exactly one of env, file, keychain, or store",
		},
		{
			name: "OTLP ambiguous header source",
			cfg: Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
				Endpoint: "https://collector.example.com:4317",
				Headers: map[string]TokenRef{
					"authorization": {Env: "AUTH", File: "/run/secrets/auth"},
				},
			}}},
			wantErr: "must reference exactly one of env, file, keychain, or store",
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

// TestConfigValidateDaemonIdentity covers #1780's DaemonIdentityConfig: the
// same exactly-one-kind, kind-specific-required-fields, fail-closed-inline-
// secret discipline RepoAuthConfig already enforces for repo-level auth.
func TestConfigValidateDaemonIdentity(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "valid pat",
			cfg:  Config{DaemonIdentity: &DaemonIdentityConfig{Kind: GitHubAuthPAT, Token: &TokenRef{Env: "DAEMON_PAT"}}},
		},
		{
			name:    "pat missing token",
			cfg:     Config{DaemonIdentity: &DaemonIdentityConfig{Kind: GitHubAuthPAT}},
			wantErr: "token must reference exactly one of env, file, keychain, or store",
		},
		{
			name: "pat token with two sources",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthPAT, Token: &TokenRef{Env: "DAEMON_PAT", File: "/f"},
			}},
			wantErr: "token must reference exactly one of env, file, keychain, or store",
		},
		{
			name: "pat rejects app fields",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthPAT, Token: &TokenRef{Env: "DAEMON_PAT"}, AppID: "123456",
			}},
			wantErr: "only valid for kind \"github-app\"",
		},
		{
			name: "pat token env must not be exposed to stages",
			cfg: Config{
				Runner:         RunnerConfig{EnvPassthrough: []string{"DAEMON_PAT"}},
				DaemonIdentity: &DaemonIdentityConfig{Kind: GitHubAuthPAT, Token: &TokenRef{Env: "DAEMON_PAT"}},
			},
			wantErr: "must not be exposed to stages",
		},
		{
			name: "valid github-app",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
				PrivateKey: &TokenRef{File: "/run/secrets/daemon-app.pem"},
			}},
		},
		{
			name: "valid github-app with slug",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42", Slug: "my-daemon",
				PrivateKey: &TokenRef{File: "/run/secrets/daemon-app.pem"},
			}},
		},
		{
			name: "github-app rejects token alongside minting",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, Token: &TokenRef{Env: "T"}, AppID: "123456", InstallationID: "42",
				PrivateKey: &TokenRef{File: "/run/secrets/daemon-app.pem"},
			}},
			wantErr: "must not configure token",
		},
		{
			name: "github-app missing appId",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, InstallationID: "42",
				PrivateKey: &TokenRef{File: "/run/secrets/daemon-app.pem"},
			}},
			wantErr: "appId is required",
		},
		{
			name: "github-app missing installationId",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, AppID: "123456",
				PrivateKey: &TokenRef{File: "/run/secrets/daemon-app.pem"},
			}},
			wantErr: "installationId is required",
		},
		{
			name: "github-app non-numeric installationId",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, AppID: "123456", InstallationID: "acme-corp",
				PrivateKey: &TokenRef{File: "/run/secrets/daemon-app.pem"},
			}},
			wantErr: "must be the numeric installation ID",
		},
		{
			name: "github-app missing privateKey",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
			}},
			wantErr: "privateKey must reference exactly one",
		},
		{
			name: "github-app privateKey env must not be exposed to stages",
			cfg: Config{
				Runner: RunnerConfig{EnvPassthrough: []string{"DAEMON_APP_KEY"}},
				DaemonIdentity: &DaemonIdentityConfig{
					Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
					PrivateKey: &TokenRef{Env: "DAEMON_APP_KEY"},
				},
			},
			wantErr: "must not be exposed to stages",
		},
		{
			name: "github-app privateKey undeclared store",
			cfg: Config{DaemonIdentity: &DaemonIdentityConfig{
				Kind: GitHubAuthApp, AppID: "123456", InstallationID: "42",
				PrivateKey: &TokenRef{Store: "missing-kv/app-key"},
			}},
			wantErr: "not declared under secretStores",
		},
		{
			name:    "unsupported kind",
			cfg:     Config{DaemonIdentity: &DaemonIdentityConfig{Kind: "oidc", Token: &TokenRef{Env: "T"}}},
			wantErr: "unsupported kind \"oidc\"",
		},
		{
			name:    "empty kind",
			cfg:     Config{DaemonIdentity: &DaemonIdentityConfig{Token: &TokenRef{Env: "T"}}},
			wantErr: "unsupported kind \"\"",
		},
		{
			name: "nil DaemonIdentity is byte-identical to before this field existed",
			cfg:  Config{},
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
			wantErr: "exactly one of env, file, keychain, or store",
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

// TestTokenRefCredentialTokenRef pins the conversion every consumer feeds
// credentials.NewResolverWithStores (#683): all three sources carry through
// under the caller's ref name, so a store-backed ref reaches the resolver
// intact (and a resolver built without store support still rejects it at
// construction — see the credentials package's own fail-closed tests).
func TestTokenRefCredentialTokenRef(t *testing.T) {
	got := TokenRef{Env: "T"}.CredentialTokenRef("repo")
	if got.Name != "repo" || got.Env != "T" || got.File != "" || got.Store != "" {
		t.Fatalf("env ref = %+v", got)
	}
	got = TokenRef{Store: "prod-kv/github-token"}.CredentialTokenRef("repo")
	if got.Name != "repo" || got.Store != "prod-kv/github-token" || got.Env != "" || got.File != "" {
		t.Fatalf("store ref = %+v", got)
	}
	if _, err := credentials.NewResolver([]credentials.TokenRef{got}); err == nil {
		t.Fatal("a store-backed ref must fail closed in a resolver built without store support")
	}
}

func TestDefaultStageTimeoutDuration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		// Zero means "unset": the executor keeps its own built-in default
		// rather than this package substituting one.
		{name: "unset", value: "", want: 0},
		{name: "duration", value: "25m", want: 25 * time.Minute},
		{name: "seconds", value: "90s", want: 90 * time.Second},
		{name: "malformed", value: "25 minutes", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "negative", value: "-5m", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RunnerConfig{DefaultStageTimeout: tc.value}.DefaultStageTimeoutDuration()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DefaultStageTimeoutDuration(%q) = %s, want an error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultStageTimeoutDuration(%q): %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("DefaultStageTimeoutDuration(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

// A malformed baseline must fail `goobers validate` once, not every run at
// dispatch — the value is consumed when the deterministic executor is built.
func TestValidateRejectsMalformedDefaultStageTimeout(t *testing.T) {
	t.Parallel()
	cfg := &Config{Runner: RunnerConfig{DefaultStageTimeout: "twenty-five minutes"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error for a malformed runner.defaultStageTimeout")
	}
	if !strings.Contains(err.Error(), "runner.defaultStageTimeout") {
		t.Fatalf("Validate() error = %q, want it to name runner.defaultStageTimeout", err)
	}
}

// TestRepoAuthBotLogin (#3343): the declared App slug derives the "[bot]"
// login trusted-comment checks compare against; non-App kinds and absent
// slugs return empty so the GET /user path stays in place for PATs.
func TestRepoAuthBotLogin(t *testing.T) {
	cases := []struct {
		name string
		auth *RepoAuthConfig
		want string
	}{
		{"app with slug", &RepoAuthConfig{Kind: GitHubAuthApp, Slug: "goobersbot"}, "goobersbot[bot]"},
		{"app with padded slug", &RepoAuthConfig{Kind: GitHubAuthApp, Slug: " goobersbot "}, "goobersbot[bot]"},
		{"app without slug", &RepoAuthConfig{Kind: GitHubAuthApp}, ""},
		{"pat kind never derives", &RepoAuthConfig{Kind: GitHubAuthPAT, Slug: "goobersbot"}, ""},
		{"nil auth", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.auth.BotLogin(); got != tc.want {
				t.Fatalf("BotLogin() = %q, want %q", got, tc.want)
			}
		})
	}
}

// #3414 F1: a GitHub App installation belongs to exactly one owner, so a single
// installationId cannot cover repos spanning several. Such a config is already
// runtime-fatal (a 422 at first cross-owner mint, #3341), so rejecting it at
// load can only hit configs that never worked.
func TestValidateDaemonIdentityOwnerCoverage(t *testing.T) {
	app := func() *DaemonIdentityConfig {
		return &DaemonIdentityConfig{
			Kind: GitHubAuthApp, AppID: "123456", InstallationID: "999",
			PrivateKey: &TokenRef{File: "/key.pem"},
		}
	}
	gh := func(owner, name string) RepoRef {
		return RepoRef{Provider: "github", Owner: owner, Name: name}
	}

	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "two owners on one installation is rejected and names both",
			cfg: Config{
				DaemonIdentity: app(),
				Repos:          []RepoRef{gh("acme", "a"), gh("globex", "b")},
			},
			wantErr: "acme, globex",
		},
		{
			name: "single owner is unchanged",
			cfg: Config{
				DaemonIdentity: app(),
				Repos:          []RepoRef{gh("acme", "a"), gh("acme", "b")},
			},
		},
		{
			// The check is about GitHub App installation scope, so an ADO
			// organization is not an owner for this purpose. Counting it would
			// reject a perfectly valid mixed-provider instance.
			name: "non-github repos do not count toward owner span",
			cfg: Config{
				DaemonIdentity: app(),
				Repos: []RepoRef{
					gh("acme", "a"),
					{Provider: "ado", Owner: "globex", Project: "p", Name: "b"},
				},
			},
		},
		{
			name: "pat daemon identity is untouched on multi-owner repos",
			cfg: Config{
				DaemonIdentity: &DaemonIdentityConfig{Kind: GitHubAuthPAT, Token: &TokenRef{Env: "DAEMON_PAT"}},
				Repos:          []RepoRef{gh("acme", "a"), gh("globex", "b")},
			},
		},
		{
			name: "no installation id declared yet is not this check's business",
			cfg: Config{
				DaemonIdentity: &DaemonIdentityConfig{Kind: GitHubAuthApp, AppID: "123456"},
				Repos:          []RepoRef{gh("acme", "a"), gh("globex", "b")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateDaemonIdentityOwnerCoverage()
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

// #3414 F1, second arm: GitHub allows one installation per (App, owner), so a
// repo naming the same App as the daemon identity but a different installation
// means one half of the config is wrong. The message must not presume which.
func TestValidateDaemonIdentitySameAppInstallations(t *testing.T) {
	identity := &DaemonIdentityConfig{
		Kind: GitHubAuthApp, AppID: "123456", InstallationID: "999",
		PrivateKey: &TokenRef{File: "/key.pem"},
	}
	repo := func(appID, installationID GitHubID) RepoRef {
		return RepoRef{
			Provider: "github", Owner: "acme", Name: "a",
			Auth: &RepoAuthConfig{Kind: GitHubAuthApp, AppID: appID, InstallationID: installationID},
		}
	}

	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "same app, different installation is rejected",
			cfg:     Config{DaemonIdentity: identity, Repos: []RepoRef{repo("123456", "888")}},
			wantErr: "disagrees with daemonIdentity.installationId",
		},
		{
			name: "same app, same installation agrees",
			cfg:  Config{DaemonIdentity: identity, Repos: []RepoRef{repo("123456", "999")}},
		},
		{
			// A different App is a different installation namespace entirely,
			// so disagreement carries no information.
			name: "different app is not cross-checked",
			cfg:  Config{DaemonIdentity: identity, Repos: []RepoRef{repo("654321", "888")}},
		},
		{
			name: "repo without its own installation id inherits and is not a conflict",
			cfg:  Config{DaemonIdentity: identity, Repos: []RepoRef{repo("123456", "")}},
		},
		{
			name: "repo without app auth is ignored",
			cfg: Config{DaemonIdentity: identity, Repos: []RepoRef{
				{Provider: "github", Owner: "acme", Name: "a", Token: TokenRef{Env: "T"}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateDaemonIdentitySameAppInstallations()
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
