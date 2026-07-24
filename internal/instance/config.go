package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runnercap"
)

// APIVersion and Kind for instance.yaml. Mirrors the config-as-code
// apiVersion/kind convention (ARCHITECTURE.md §6) though instance.yaml is a
// provisioning file, never a CR the operator reconciles.
const (
	ConfigAPIVersion                 = "goobers.dev/v1alpha1"
	ConfigKind                       = "Instance"
	DefaultAPIListenAddress          = "127.0.0.1:8080"
	DefaultWebhookListenAddress      = "127.0.0.1:8081"
	OTLPEndpointEnv                  = "GOOBERS_OTLP_ENDPOINT"
	OTLPInsecureEnv                  = "GOOBERS_OTLP_INSECURE"
	DefaultWorkflowSourceRef         = "main"
	WorkflowSourceKindLocalDir       = "local-dir"
	WorkflowSourceKindGit            = "git"
	DefaultDaemonLivenessTimeout     = 2 * time.Minute
	MinimumDaemonLivenessTimeout     = 2 * time.Second
	DefaultStalledRunTimeout         = 45 * time.Minute
	DefaultClaimsLockTimeout         = 30 * time.Second
	DefaultTelemetryRetentionWindow  = 90 * 24 * time.Hour
	DefaultTelemetryRetentionMaxRuns = 500
)

// Config is the parsed instance.yaml: target repo(s) + provider, token source
// refs, telemetry settings, instance-level run conditions (INST-010), and the
// timezone cron schedules evaluate in (issue #137 — previously promised by
// internal/localscheduler's own doc comments but never actually a field
// anywhere, so every schedule silently ran in whatever the host process's
// local zone happened to be).
type Config struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind" yaml:"kind"`
	Repos      []RepoRef `json:"repos" yaml:"repos"`
	// WorkflowSource locates the definitions-as-code tree independently of the
	// target code repositories. Nil keeps the local <instance-root>/config
	// default.
	WorkflowSource *WorkflowSource `json:"workflowSource,omitempty" yaml:"workflowSource,omitempty"`
	API            APIConfig       `json:"api,omitempty" yaml:"api,omitempty"`
	Webhook        WebhookConfig   `json:"webhook,omitempty" yaml:"webhook,omitempty"`
	Telemetry      TelemetryConfig `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	RunConditions  RunConditions   `json:"runConditions,omitempty" yaml:"runConditions,omitempty"`
	Retention      RetentionConfig `json:"retention,omitempty" yaml:"retention,omitempty"`
	// Notifications opts `goobers up` into native desktop notifications for
	// escalated and failed runs. It defaults to false.
	Notifications bool `json:"notifications,omitempty" yaml:"notifications,omitempty"`
	// Credentials sources individual stage capabilities from their own token refs,
	// beyond the default of backing every credentialed capability with the
	// first repo's token (#287, multi-token credentials). Each entry points one
	// capability at a distinct token; an entry for a capability the runner would
	// otherwise default-grant to the repo token OVERRIDES that grant. This is
	// what lets an agentic stage carry a personal "Copilot Requests" PAT for the
	// model (agent:model) alongside the org-repo token for the github tool.
	Credentials []CredentialGrant `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// Timezone is an IANA location name (e.g. "America/New_York") every
	// workflow's cron schedule evaluates in. Empty defaults to UTC — a fixed,
	// reproducible default independent of the host process's own local zone,
	// which would otherwise vary by deployment and isn't itself DST-free.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Runner declares this local runner's static capability claims (RRQ-1,
	// #1101): the toolchains and host properties it advertises as preinstalled
	// (e.g. dotnet@8, xcode, os=windows). A gaggle/stage that requires a
	// capability this runner does not claim fails to schedule with a diagnostic
	// naming it (docs/design/v1/polyglot-stacks.md §5). Empty claims nothing, so
	// a Go-only instance that declares no requirements is unaffected.
	Runner RunnerConfig `json:"runner,omitempty" yaml:"runner,omitempty"`
	// SecretStores declares named external secret stores token refs can resolve
	// through (config half of #683, SEC-010). A token ref opts in per ref with
	// store: "<storeName>/<secretName>"; an instance that declares no stores and
	// uses only env/file refs behaves byte-identically to before this field
	// existed.
	SecretStores []SecretStoreConfig `json:"secretStores,omitempty" yaml:"secretStores,omitempty"`
}

// WorkflowSource locates the workflow configuration independently of Repos.
// A local-dir source reads Path directly. A git source reads a committed Ref
// from either a local repository Path or a remote HTTPS URL; remote sources
// require their own token reference.
type WorkflowSource struct {
	Kind  string    `json:"kind" yaml:"kind"`
	Path  string    `json:"path,omitempty" yaml:"path,omitempty"`
	URL   string    `json:"url,omitempty" yaml:"url,omitempty"`
	Ref   string    `json:"ref,omitempty" yaml:"ref,omitempty"`
	Token *TokenRef `json:"token,omitempty" yaml:"token,omitempty"`
}

// TrackedRef returns the configured git ref, defaulting to main.
func (s WorkflowSource) TrackedRef() string {
	if s.Kind != WorkflowSourceKindGit {
		return ""
	}
	if s.Ref == "" {
		return DefaultWorkflowSourceRef
	}
	return s.Ref
}

// RunnerConfig declares the local runner's static, advertised capability set
// (RRQ-1, #1101). Capabilities are free-form toolchain/platform tokens
// (`dotnet@8`, `xcode`, `os=windows`) — see internal/runnercap for the
// vocabulary and why they are distinct from credential capabilities.
type RunnerConfig struct {
	// Capabilities are the toolchain/platform capabilities this runner claims
	// are preinstalled. The scheduler admits a run only when the runner claims
	// every capability the run's gaggle and stages require.
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// EnvPassthrough names additional ambient env vars carried from the daemon
	// process into every deterministic stage and harness subprocess, on top of
	// the built-in default-deny allowlist (internal/procenv, #736). It is the
	// escape hatch for a custom toolchain whose env var the built-in list does
	// not cover — e.g. a private `NUGET_CONFIG_FILE` or a bespoke `FOO_HOME` —
	// so a team does not need a Goobers code change to pass its own var through.
	// Each entry must be a well-formed env var name (procenv.ValidName); this
	// stays default-deny — an explicit opt-in list of names, never os.Environ()
	// passthrough — and declaring a name whose var is unset is a harmless no-op.
	EnvPassthrough []string `json:"envPassthrough,omitempty" yaml:"envPassthrough,omitempty"`
	// LivenessTimeout is the maximum age of the scheduler tick heartbeat before
	// the daemon is reported unhealthy. Empty defaults to two minutes.
	LivenessTimeout string `json:"livenessTimeout,omitempty" yaml:"livenessTimeout,omitempty"`
}

// APIConfig configures the daemon's read-only HTTP API.
type APIConfig struct {
	// Listen is a host:port address. Only loopback hosts are accepted.
	Listen string `json:"listen,omitempty" yaml:"listen,omitempty"`
}

// WebhookConfig configures the optional GitHub webhook receiver. The daemon
// starts this listener only when Secret is configured and at least one workflow
// declares a webhook trigger.
type WebhookConfig struct {
	// Listen is a host:port address. Only loopback hosts are accepted.
	Listen string `json:"listen,omitempty" yaml:"listen,omitempty"`
	// Secret references the instance-wide GitHub webhook secret.
	Secret TokenRef `json:"secret,omitempty" yaml:"secret,omitempty"`
}

// RepoRef is a target repository this instance connects to.
type RepoRef struct {
	// Provider is the backing system: "github" or "ado".
	Provider string `json:"provider" yaml:"provider"`
	// Owner is the GitHub owner or Azure DevOps organization.
	Owner string `json:"owner" yaml:"owner"`
	// Project is required for Azure DevOps and omitted for GitHub.
	Project string `json:"project,omitempty" yaml:"project,omitempty"`
	// Name is the repo name.
	Name string `json:"name" yaml:"name"`
	// Token is a reference to this repo's credential. Never an inline value
	// (CFG-009, SEC-010). GitHub and ADO PAT auth require exactly one of Env
	// or File. Entra-backed ADO auth does not use this field.
	Token TokenRef `json:"token,omitempty" yaml:"token,omitempty"`
	// Auth selects Azure DevOps authentication. Nil preserves legacy PAT
	// behavior when Token is configured.
	Auth *ADOAuthConfig `json:"auth,omitempty" yaml:"auth,omitempty"`
}

const (
	// ADOAuthPAT selects an env/file-backed personal access token.
	ADOAuthPAT = "pat"
	// ADOAuthAzureCLI selects the current local Azure CLI login.
	ADOAuthAzureCLI = "azure-cli"
	// ADOAuthWorkloadIdentity selects federated Azure workload identity.
	ADOAuthWorkloadIdentity = "workload-identity"
	// ADOAuthManagedIdentity selects an Azure managed identity.
	ADOAuthManagedIdentity = "managed-identity"
)

// ADOAuthConfig selects an Azure DevOps credential source without embedding
// credential material in configuration.
type ADOAuthConfig struct {
	Kind string `json:"kind" yaml:"kind"`
	// Tenant optionally pins Azure CLI authentication to one tenant.
	Tenant string `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	// ClientID optionally selects a user-assigned managed identity.
	ClientID string `json:"clientId,omitempty" yaml:"clientId,omitempty"`
}

const (
	// SecretStoreKindAzureKeyVault is the only supported secret store kind
	// today (SEC-010); the seam is vendor-neutral by name+kind indirection.
	SecretStoreKindAzureKeyVault = "azure-key-vault"
	// SecretStoreAuthWorkloadIdentity selects federated Azure workload identity.
	SecretStoreAuthWorkloadIdentity = "workload-identity"
	// SecretStoreAuthManagedIdentity selects an Azure managed identity.
	SecretStoreAuthManagedIdentity = "managed-identity"
	// SecretStoreAuthAzureCLI selects the current local Azure CLI login.
	SecretStoreAuthAzureCLI = "azure-cli"
)

// SecretStoreConfig declares one named external secret store (#683). Token
// refs opt in per ref via store: "<name>/<secretName>"; declaring a store a
// ref never uses is harmless. Auth to the store itself always uses an ambient
// identity chain — never a token ref, which would be circular.
type SecretStoreConfig struct {
	// Name is the handle store-backed token refs address this store by.
	// DNS-label shaped so it can never be confused with the "/"-separated
	// secret name that follows it in a ref.
	Name string `json:"name" yaml:"name"`
	// Kind is the store vendor; only "azure-key-vault" is supported.
	Kind string `json:"kind" yaml:"kind"`
	// VaultURI is the https vault endpoint, e.g. "https://acme.vault.azure.net".
	VaultURI string `json:"vaultURI" yaml:"vaultURI"`
	// Auth selects how this process authenticates to the store.
	Auth *SecretStoreAuthConfig `json:"auth" yaml:"auth"`
	// CacheTTLSeconds bounds the in-memory cache of resolved secrets so
	// rotation in the store is picked up without hammering it per resolve.
	// Zero/omitted leaves the resolver's default in effect.
	CacheTTLSeconds int `json:"cacheTTLSeconds,omitempty" yaml:"cacheTTLSeconds,omitempty"`
}

// SecretStoreAuthConfig selects the ambient identity used to reach a secret
// store, mirroring ADOAuthConfig: a source selector, never credential material.
type SecretStoreAuthConfig struct {
	Kind string `json:"kind" yaml:"kind"`
	// ClientID optionally pins a user-assigned identity. Valid for
	// workload-identity and managed-identity; azure-cli has no client to pin.
	ClientID string `json:"clientId,omitempty" yaml:"clientId,omitempty"`
}

// TokenRef points at a credential without storing its value: an environment
// variable name, a path to a file containing it (SEC-*, "Env vars / token
// file" at tiers 1-2, ARCHITECTURE.md §9), or a secret in a declared external
// secret store (#683). Exactly one source per ref.
type TokenRef struct {
	// Env is the name of an environment variable holding the token.
	Env string `json:"env,omitempty" yaml:"env,omitempty"`
	// File is a path to a file whose contents are the token.
	File string `json:"file,omitempty" yaml:"file,omitempty"`
	// Store references a secret in a declared secretStores entry as
	// "<storeName>/<secretName>". The store name must match a secretStores
	// entry; the secret name is interpreted by that store's resolver.
	Store string `json:"store,omitempty" yaml:"store,omitempty"`
}

// sourceCount reports how many of the ref's mutually-exclusive sources are set.
func (r TokenRef) sourceCount() int {
	n := 0
	if r.Env != "" {
		n++
	}
	if r.File != "" {
		n++
	}
	if r.Store != "" {
		n++
	}
	return n
}

// Configured reports whether any token source is set.
func (r TokenRef) Configured() bool {
	return r.sourceCount() > 0
}

// EnvFileSources returns the local env/file halves of the ref for consumers
// that resolve only local sources. It fails closed on a store-backed ref: the
// secretStores config surface (#683) is ahead of resolver wiring, so a store
// ref reaching a local-only build path must be rejected with a diagnostic
// naming it, never silently read as an unconfigured ref.
func (r TokenRef) EnvFileSources() (env, file string, err error) {
	if r.Store != "" {
		return "", "", fmt.Errorf(
			"token ref store %q: secret store resolution is not configured in this build path yet", r.Store)
	}
	return r.Env, r.File, nil
}

// CredentialGrant sources one stage capability from its own token ref (#287).
// Runner-owned capabilities use their dedicated config surfaces instead.
type CredentialGrant struct {
	// Capability is the canonical capability string (internal/capability) this
	// token backs, e.g. "agent:model" or "repo:push" (to override the default).
	Capability string `json:"capability" yaml:"capability"`
	// Token is the source of the credential — exactly one of env or file, like
	// a repo's token; inline secret values are never permitted.
	Token TokenRef `json:"token" yaml:"token"`
}

// TelemetryConfig configures the local telemetry rollup store and optional
// collector push (§8).
type TelemetryConfig struct {
	// Enabled toggles OTel client construction, span emission, local SQLite
	// ingest, and configured collector push. Defaults to true.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// OTLP opts into pushing the same spans to an OTLP/gRPC collector.
	OTLP *OTLPConfig `json:"otlp,omitempty" yaml:"otlp,omitempty"`
	// Retention bounds terminal run journals and their rollup rows. Automatic
	// daemon pruning is opt-in; explicit pruning can use the configured policy
	// while automation remains disabled.
	Retention *TelemetryRetentionConfig `json:"retention,omitempty" yaml:"retention,omitempty"`
}

// TelemetryRetentionConfig controls pruning of terminal run telemetry.
type TelemetryRetentionConfig struct {
	Enabled bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Window  string `json:"window,omitempty" yaml:"window,omitempty"`
	MaxRuns int    `json:"maxRuns,omitempty" yaml:"maxRuns,omitempty"`
}

// WindowDuration returns the configured retention window. Empty uses 90 days.
func (c TelemetryRetentionConfig) WindowDuration() (time.Duration, error) {
	if c.Window == "" {
		return DefaultTelemetryRetentionWindow, nil
	}
	value := c.Window
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("telemetry.retention.window %q must be a duration or whole number of days", value)
		}
		const maxDurationDays = (1<<63 - 1) / int64(24*time.Hour)
		if days <= 0 || days > maxDurationDays {
			return 0, fmt.Errorf("telemetry.retention.window must be positive, got %s", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	window, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("telemetry.retention.window %q: %w", value, err)
	}
	if window <= 0 {
		return 0, fmt.Errorf("telemetry.retention.window must be positive, got %s", window)
	}
	return window, nil
}

// MaxRunLimit returns the configured maximum retained run count. Zero uses 500.
func (c TelemetryRetentionConfig) MaxRunLimit() int {
	if c.MaxRuns == 0 {
		return DefaultTelemetryRetentionMaxRuns
	}
	return c.MaxRuns
}

// OTLPConfig configures an optional OTLP/gRPC collector. Endpoint absence
// disables collector push. Header values are always indirect secret refs.
type OTLPConfig struct {
	Endpoint string              `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Insecure bool                `json:"insecure,omitempty" yaml:"insecure,omitempty"`
	Headers  map[string]TokenRef `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// RunConditions are instance-level run conditions (§7): max parallel runs and
// per-workflow run budgets.
type RunConditions struct {
	MaxParallelRuns int            `json:"maxParallelRuns,omitempty" yaml:"maxParallelRuns,omitempty"`
	WorkflowBudgets map[string]int `json:"workflowBudgets,omitempty" yaml:"workflowBudgets,omitempty"`
	// WorkflowDailyBudgets overrides a named workflow's runs-per-day budget
	// (#340), mirroring WorkflowBudgets' per-hour override.
	WorkflowDailyBudgets map[string]int `json:"workflowDailyBudgets,omitempty" yaml:"workflowDailyBudgets,omitempty"`
	// StalledRunTimeout is the maximum period a running journal may remain
	// silent before the daemon escalates it. Empty defaults to 45 minutes.
	StalledRunTimeout string `json:"stalledRunTimeout,omitempty" yaml:"stalledRunTimeout,omitempty"`
	// ClaimsLockTimeout bounds cross-process claim-ledger lock acquisition.
	// Empty defaults to 30 seconds.
	ClaimsLockTimeout string `json:"claimsLockTimeout,omitempty" yaml:"claimsLockTimeout,omitempty"`
}

// RetentionConfig controls opt-in pruning of retained failure worktrees and
// merged local run branches. Both Enabled and DryRun default to false.
type RetentionConfig struct {
	Enabled                  bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	DryRun                   bool   `json:"dryRun,omitempty" yaml:"dryRun,omitempty"`
	MaxRetainedWorktreeBytes int64  `json:"maxRetainedWorktreeBytes,omitempty" yaml:"maxRetainedWorktreeBytes,omitempty"`
	RetainedWorktreeMaxAge   string `json:"retainedWorktreeMaxAge,omitempty" yaml:"retainedWorktreeMaxAge,omitempty"`
}

// RetainedWorktreeMaxAgeDuration resolves the optional retention window.
// Zero disables age-based pruning.
func (c RetentionConfig) RetainedWorktreeMaxAgeDuration() (time.Duration, error) {
	if c.RetainedWorktreeMaxAge == "" {
		return 0, nil
	}
	window, err := time.ParseDuration(c.RetainedWorktreeMaxAge)
	if err != nil {
		return 0, fmt.Errorf("retention.retainedWorktreeMaxAge %q: %w", c.RetainedWorktreeMaxAge, err)
	}
	if window <= 0 {
		return 0, fmt.Errorf("retention.retainedWorktreeMaxAge must be positive, got %s", window)
	}
	return window, nil
}

// StalledRunTimeoutDuration resolves the configured stalled-run deadline.
func (c RunConditions) StalledRunTimeoutDuration() (time.Duration, error) {
	if c.StalledRunTimeout == "" {
		return DefaultStalledRunTimeout, nil
	}
	timeout, err := time.ParseDuration(c.StalledRunTimeout)
	if err != nil {
		return 0, fmt.Errorf("runConditions.stalledRunTimeout %q: %w", c.StalledRunTimeout, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("runConditions.stalledRunTimeout must be positive, got %s", timeout)
	}
	return timeout, nil
}

// ClaimsLockTimeoutDuration resolves the configured claims-lock deadline.
func (c RunConditions) ClaimsLockTimeoutDuration() (time.Duration, error) {
	if c.ClaimsLockTimeout == "" {
		return DefaultClaimsLockTimeout, nil
	}
	timeout, err := time.ParseDuration(c.ClaimsLockTimeout)
	if err != nil {
		return 0, fmt.Errorf("runConditions.claimsLockTimeout %q: %w", c.ClaimsLockTimeout, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("runConditions.claimsLockTimeout must be positive, got %s", timeout)
	}
	return timeout, nil
}

// LivenessTimeoutDuration resolves the configured daemon heartbeat deadline.
func (c RunnerConfig) LivenessTimeoutDuration() (time.Duration, error) {
	if c.LivenessTimeout == "" {
		return DefaultDaemonLivenessTimeout, nil
	}
	timeout, err := time.ParseDuration(c.LivenessTimeout)
	if err != nil {
		return 0, fmt.Errorf("runner.livenessTimeout %q: %w", c.LivenessTimeout, err)
	}
	if timeout < MinimumDaemonLivenessTimeout {
		return 0, fmt.Errorf("runner.livenessTimeout must be at least %s, got %s", MinimumDaemonLivenessTimeout, timeout)
	}
	return timeout, nil
}

// TelemetryEnabled reports whether the local rollup store is enabled
// (defaults to true when unset). Wired into cmd/goobers' up.go/run.go (issue
// #129): telemetry.enabled was documented and set in the real self-hosting
// config (selfhost/instance.yaml.example) but had zero callers.
func (c *Config) TelemetryEnabled() bool {
	return c.Telemetry.Enabled == nil || *c.Telemetry.Enabled
}

// ResolveOTLPConfig applies process environment overrides to instance.yaml and
// validates the resulting collector configuration.
func (c *Config) ResolveOTLPConfig(lookupEnv func(string) (string, bool)) (OTLPConfig, error) {
	var resolved OTLPConfig
	if c.Telemetry.OTLP != nil {
		resolved = *c.Telemetry.OTLP
	}
	if endpoint, ok := lookupEnv(OTLPEndpointEnv); ok {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			return OTLPConfig{}, fmt.Errorf("%s must not be empty when set", OTLPEndpointEnv)
		}
		resolved.Endpoint = endpoint
	}
	if raw, ok := lookupEnv(OTLPInsecureEnv); ok {
		raw = strings.TrimSpace(raw)
		if !strings.EqualFold(raw, "true") && !strings.EqualFold(raw, "false") {
			return OTLPConfig{}, fmt.Errorf("%s must be true or false", OTLPInsecureEnv)
		}
		resolved.Insecure = strings.EqualFold(raw, "true")
	}
	if err := resolved.Validate(); err != nil {
		return OTLPConfig{}, fmt.Errorf("telemetry.otlp: %w", err)
	}
	if resolved.Enabled() && !c.TelemetryEnabled() {
		return OTLPConfig{}, fmt.Errorf("telemetry.otlp.endpoint cannot be set when telemetry.enabled is false")
	}
	return resolved, nil
}

// Enabled reports whether collector push is configured.
func (c OTLPConfig) Enabled() bool {
	return c.Endpoint != ""
}

// Validate checks the collector endpoint, transport, and credential references.
func (c OTLPConfig) Validate() error {
	if c.Endpoint == "" {
		if c.Insecure || len(c.Headers) != 0 {
			return fmt.Errorf("endpoint is required when insecure mode or headers are configured")
		}
		return nil
	}
	if strings.TrimSpace(c.Endpoint) != c.Endpoint {
		return fmt.Errorf("endpoint must not contain leading or trailing whitespace")
	}
	if err := validateOTLPEndpoint(c.Endpoint, c.Insecure); err != nil {
		return fmt.Errorf("endpoint %q: %w", c.Endpoint, err)
	}
	seenHeaders := make(map[string]bool, len(c.Headers))
	for name, ref := range c.Headers {
		if !validHeaderName(name) {
			return fmt.Errorf("headers: invalid header name %q", name)
		}
		canonicalName := strings.ToLower(name)
		if seenHeaders[canonicalName] {
			return fmt.Errorf("headers: header name %q is configured more than once", name)
		}
		seenHeaders[canonicalName] = true
		if ref.sourceCount() != 1 {
			return fmt.Errorf("headers[%q] must reference exactly one of env, file, or store; inline values are not permitted", name)
		}
	}
	return nil
}

// APIListenAddress returns the configured HTTP address, defaulting to a
// loopback-only listener.
func (c *Config) APIListenAddress() string {
	if c.API.Listen == "" {
		return DefaultAPIListenAddress
	}
	return c.API.Listen
}

// WebhookListenAddress returns the configured webhook address, defaulting to a
// separate loopback-only listener.
func (c *Config) WebhookListenAddress() string {
	if c.Webhook.Listen == "" {
		return DefaultWebhookListenAddress
	}
	return c.Webhook.Listen
}

// WebhookSecretConfigured reports whether any supported secret source is
// present. Validate rejects a ref that sets more than one.
func (c *Config) WebhookSecretConfigured() bool {
	return c.Webhook.Secret.Configured()
}

// Location resolves Timezone to a *time.Location, defaulting to UTC when
// unset. Validate already rejects an unresolvable Timezone at load time, so
// this only errors if tzdata disappeared from underneath an already-loaded
// instance (e.g. between validate and use) — callers can treat a non-nil
// error here as exceptional.
func (c *Config) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", c.Timezone, err)
	}
	return loc, nil
}

// LoadConfig reads and validates instance.yaml at path. Decoding is strict:
// unknown fields (including an inline secret value under a token ref) are
// rejected rather than silently ignored.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w (instance.yaml accepts only known fields; token refs must be "+
			"token.env, token.file, or token.store — inline secret values are not permitted, CFG-009/SEC-010)", path, err)
	}
	resolvedOTLP, err := cfg.ResolveOTLPConfig(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Telemetry.OTLP != nil || resolvedOTLP.Enabled() {
		cfg.Telemetry.OTLP = &resolvedOTLP
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks instance.yaml-level invariants: known provider, non-empty
// owner/name, exactly one token source per repo, and (if set) a resolvable
// IANA timezone — fail closed at load time rather than at the first cron
// tick that tries to use it.
func (c *Config) Validate() error {
	if err := validateAPIListenAddress(c.APIListenAddress()); err != nil {
		return fmt.Errorf("api.listen: %w", err)
	}
	if c.WorkflowSource != nil {
		if err := c.WorkflowSource.Validate(); err != nil {
			return fmt.Errorf("workflowSource: %w", err)
		}
	}
	if err := validateLoopbackListenAddress(c.WebhookListenAddress()); err != nil {
		return fmt.Errorf("webhook.listen: %w", err)
	}
	// Secret stores validate before any token ref so a store-backed ref can be
	// checked against the declared store names below.
	stores, err := c.validateSecretStores()
	if err != nil {
		return err
	}
	if c.Webhook.Secret.sourceCount() > 1 {
		return fmt.Errorf("webhook.secret must reference exactly one of env, file, or store — inline secret values are never permitted (CFG-009, SEC-010)")
	}
	if err := validateStoreRef("webhook.secret", c.Webhook.Secret, stores); err != nil {
		return err
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("timezone %q: %w", c.Timezone, err)
		}
	}
	if c.Telemetry.OTLP != nil {
		if err := c.Telemetry.OTLP.Validate(); err != nil {
			return fmt.Errorf("telemetry.otlp: %w", err)
		}
		if c.Telemetry.OTLP.Enabled() && !c.TelemetryEnabled() {
			return fmt.Errorf("telemetry.otlp.endpoint cannot be set when telemetry.enabled is false")
		}
		names := make([]string, 0, len(c.Telemetry.OTLP.Headers))
		for name := range c.Telemetry.OTLP.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := validateStoreRef(fmt.Sprintf("telemetry.otlp.headers[%q]", name), c.Telemetry.OTLP.Headers[name], stores); err != nil {
				return err
			}
		}
	}
	if c.Telemetry.Retention != nil {
		if _, err := c.Telemetry.Retention.WindowDuration(); err != nil {
			return err
		}
		if c.Telemetry.Retention.MaxRuns < 0 {
			return fmt.Errorf("telemetry.retention.maxRuns must not be negative")
		}
	}
	if _, err := c.RunConditions.StalledRunTimeoutDuration(); err != nil {
		return err
	}
	if _, err := c.RunConditions.ClaimsLockTimeoutDuration(); err != nil {
		return err
	}
	if c.Retention.MaxRetainedWorktreeBytes < 0 {
		return fmt.Errorf("retention.maxRetainedWorktreeBytes must not be negative")
	}
	if _, err := c.Retention.RetainedWorktreeMaxAgeDuration(); err != nil {
		return err
	}
	for i, r := range c.Repos {
		if r.Provider != "github" && r.Provider != "ado" {
			return fmt.Errorf("repos[%d]: unsupported provider %q (supported: \"github\", \"ado\")", i, r.Provider)
		}
		if r.Owner == "" || r.Name == "" {
			return fmt.Errorf("repos[%d]: owner and name are required", i)
		}
		if r.Token.sourceCount() > 1 {
			return fmt.Errorf("repos[%d] (%s/%s): token must reference exactly one of env, file, or store — "+
				"inline secret values are never permitted (CFG-009, SEC-010)", i, r.Owner, r.Name)
		}
		if err := validateStoreRef(fmt.Sprintf("repos[%d] (%s/%s): token", i, r.Owner, r.Name), r.Token, stores); err != nil {
			return err
		}
		switch r.Provider {
		case "github":
			if r.Project != "" {
				return fmt.Errorf("repos[%d] (%s/%s): project is only valid for provider \"ado\"", i, r.Owner, r.Name)
			}
			if r.Auth != nil {
				return fmt.Errorf("repos[%d] (%s/%s): auth is only valid for provider \"ado\"", i, r.Owner, r.Name)
			}
			if !r.Token.Configured() {
				return fmt.Errorf("repos[%d] (%s/%s): token must reference exactly one of env, file, or store — "+
					"inline secret values are never permitted (CFG-009, SEC-010)", i, r.Owner, r.Name)
			}
		case "ado":
			if r.Project == "" {
				return fmt.Errorf("repos[%d] (%s/%s): project is required for provider \"ado\"", i, r.Owner, r.Name)
			}
			kind := ADOAuthPAT
			if r.Auth != nil {
				kind = r.Auth.Kind
			}
			switch kind {
			case ADOAuthPAT:
				if !r.Token.Configured() {
					return fmt.Errorf("repos[%d] (%s/%s): ADO PAT auth requires token.env, token.file, or token.store", i, r.Owner, r.Name)
				}
			case ADOAuthAzureCLI, ADOAuthWorkloadIdentity, ADOAuthManagedIdentity:
				if r.Token.Configured() {
					return fmt.Errorf("repos[%d] (%s/%s): ADO auth kind %q must not configure token.env, token.file, or token.store", i, r.Owner, r.Name, kind)
				}
			default:
				return fmt.Errorf("repos[%d] (%s/%s): unsupported ADO auth kind %q", i, r.Owner, r.Name, kind)
			}
			if r.Auth != nil && r.Auth.ClientID != "" && kind != ADOAuthManagedIdentity {
				return fmt.Errorf("repos[%d] (%s/%s): auth.clientId is only valid for managed-identity", i, r.Owner, r.Name)
			}
		}
	}
	seen := make(map[string]bool, len(c.Credentials))
	for i, cg := range c.Credentials {
		// Fail closed at load, not at the first stage that tries to resolve a
		// bad grant (#287): an unknown capability is a typo the compiler would
		// never see (credentials: isn't a workflow), and a token ref that names
		// neither/both of env|file can never resolve.
		if !capability.Known(cg.Capability) {
			return fmt.Errorf("credentials[%d]: unknown capability %q", i, cg.Capability)
		}
		if !capability.StageDeclarable(cg.Capability) {
			return fmt.Errorf(
				"credentials[%d]: capability %q is runner-owned; configure it through workflowSource.token",
				i,
				cg.Capability,
			)
		}
		if seen[cg.Capability] {
			return fmt.Errorf("credentials[%d]: capability %q is sourced more than once", i, cg.Capability)
		}
		seen[cg.Capability] = true
		if cg.Token.sourceCount() != 1 {
			return fmt.Errorf("credentials[%d] (%s): token must reference exactly one of env, file, or store — "+
				"inline secret values are never permitted (CFG-009, SEC-010)", i, cg.Capability)
		}
		if err := validateStoreRef(fmt.Sprintf("credentials[%d] (%s): token", i, cg.Capability), cg.Token, stores); err != nil {
			return err
		}
	}
	// Fail closed at load on a malformed runner capability claim (RRQ-1): a
	// claim that can never string-match a requirement is a typo the scheduler
	// would otherwise turn into an every-run schedule refusal at 3am, not a
	// startup error. Duplicates collapse harmlessly (set membership), so only
	// the token shape is enforced here.
	for i, c := range c.Runner.Capabilities {
		if err := runnercap.ValidateToken(c); err != nil {
			return fmt.Errorf("runner.capabilities[%d]: %w", i, err)
		}
	}
	if _, err := c.Runner.LivenessTimeoutDuration(); err != nil {
		return err
	}
	// Fail closed at load on a malformed env-passthrough name (#736): a name
	// carrying '=', NUL, or shell metacharacters could never be a real env var
	// and, unchecked, would silently mis-split at stage launch. Default-deny is
	// unaffected — this only validates the shape of an explicit opt-in name.
	for i, name := range c.Runner.EnvPassthrough {
		if !procenv.ValidName(name) {
			return fmt.Errorf("runner.envPassthrough[%d]: %q is not a valid environment variable name", i, name)
		}
	}
	if c.WorkflowSource != nil && c.WorkflowSource.Token != nil {
		if err := validateStoreRef("workflowSource.token", *c.WorkflowSource.Token, stores); err != nil {
			return err
		}
	}
	if c.WorkflowSource != nil &&
		c.WorkflowSource.Token != nil &&
		c.WorkflowSource.Token.Env != "" &&
		stageEnvironmentAllows(c.WorkflowSource.Token.Env, c.Runner.EnvPassthrough) {
		return fmt.Errorf(
			"workflowSource.token.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
			c.WorkflowSource.Token.Env,
		)
	}
	return nil
}

// validateSecretStores checks every secretStores entry fail-closed at load
// (#683): a malformed store is a typo nothing later could resolve, and the
// scheduler-time alternative is an opaque credential failure mid-run. Returns
// the set of declared store names for store-ref checks.
func (c *Config) validateSecretStores() (map[string]bool, error) {
	if len(c.SecretStores) == 0 {
		return nil, nil
	}
	stores := make(map[string]bool, len(c.SecretStores))
	for i, s := range c.SecretStores {
		if s.Name == "" {
			return nil, fmt.Errorf("secretStores[%d]: name is required", i)
		}
		if !validSecretStoreName(s.Name) {
			return nil, fmt.Errorf("secretStores[%d]: name %q must be a lowercase DNS label (letters, digits, and interior hyphens, at most 63 characters)", i, s.Name)
		}
		if stores[s.Name] {
			return nil, fmt.Errorf("secretStores[%d]: name %q is declared more than once", i, s.Name)
		}
		stores[s.Name] = true
		if s.Kind != SecretStoreKindAzureKeyVault {
			return nil, fmt.Errorf("secretStores[%d] (%s): unsupported kind %q (supported: %q)", i, s.Name, s.Kind, SecretStoreKindAzureKeyVault)
		}
		if err := validateVaultURI(s.VaultURI); err != nil {
			return nil, fmt.Errorf("secretStores[%d] (%s): vaultURI: %w", i, s.Name, err)
		}
		if s.Auth == nil {
			return nil, fmt.Errorf("secretStores[%d] (%s): auth is required (kind: one of %q, %q, %q) — store access always authenticates through an ambient identity, never a token ref",
				i, s.Name, SecretStoreAuthWorkloadIdentity, SecretStoreAuthManagedIdentity, SecretStoreAuthAzureCLI)
		}
		switch s.Auth.Kind {
		case SecretStoreAuthWorkloadIdentity, SecretStoreAuthManagedIdentity:
		case SecretStoreAuthAzureCLI:
			if s.Auth.ClientID != "" {
				return nil, fmt.Errorf("secretStores[%d] (%s): auth.clientId is not valid for auth kind %q", i, s.Name, s.Auth.Kind)
			}
		default:
			return nil, fmt.Errorf("secretStores[%d] (%s): unsupported auth kind %q (supported: %q, %q, %q)",
				i, s.Name, s.Auth.Kind, SecretStoreAuthWorkloadIdentity, SecretStoreAuthManagedIdentity, SecretStoreAuthAzureCLI)
		}
		if s.CacheTTLSeconds < 0 {
			return nil, fmt.Errorf("secretStores[%d] (%s): cacheTTLSeconds must not be negative", i, s.Name)
		}
	}
	return stores, nil
}

// validateStoreRef checks a store-backed token ref's "<storeName>/<secretName>"
// format and that it names a declared secretStores entry. A ref with no store
// half passes untouched; scope names the field for the error message.
func validateStoreRef(scope string, ref TokenRef, stores map[string]bool) error {
	if ref.Store == "" {
		return nil
	}
	name, secret, ok := strings.Cut(ref.Store, "/")
	if !ok || name == "" || secret == "" || strings.Contains(secret, "/") {
		return fmt.Errorf("%s: store ref %q must have the form \"<storeName>/<secretName>\"", scope, ref.Store)
	}
	if !stores[name] {
		return fmt.Errorf("%s: store ref %q names secret store %q, which is not declared under secretStores", scope, ref.Store, name)
	}
	return nil
}

// validSecretStoreName reports whether name is a lowercase DNS label: it can
// never carry the "/" separator or shell/URL metacharacters, so a store ref
// always splits unambiguously.
func validSecretStoreName(name string) bool {
	if len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return len(name) > 0
}

// validateVaultURI checks an Azure Key Vault endpoint: https, host only.
func validateVaultURI(raw string) error {
	if raw == "" {
		return fmt.Errorf("is required for kind %q", SecretStoreKindAzureKeyVault)
	}
	if strings.TrimSpace(raw) != raw {
		return fmt.Errorf("must not contain leading or trailing whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("host is required")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("userinfo, paths, queries, and fragments are not supported")
	}
	return nil
}

// Validate checks workflow-source shape without resolving credentials or
// accessing the source.
func (s WorkflowSource) Validate() error {
	hasPath := s.Path != ""
	hasURL := s.URL != ""

	switch s.Kind {
	case WorkflowSourceKindLocalDir:
		if !hasPath {
			return fmt.Errorf("path is required for kind %q", s.Kind)
		}
		if hasURL || s.Ref != "" || s.Token != nil {
			return fmt.Errorf("kind %q accepts only path", s.Kind)
		}
	case WorkflowSourceKindGit:
		if hasPath == hasURL {
			return fmt.Errorf("kind %q must set exactly one of path or url", s.Kind)
		}
		if hasURL {
			if err := validateRemoteGitURL(s.URL); err != nil {
				return err
			}
			if s.Token == nil || s.Token.sourceCount() != 1 {
				return fmt.Errorf("remote git token must reference exactly one of env, file, or store — inline secret values are never permitted (CFG-009, SEC-010)")
			}
		} else if s.Token != nil {
			return fmt.Errorf("token is only valid for a remote git url")
		}
	default:
		return fmt.Errorf("unsupported kind %q (supported: \"local-dir\", \"git\")", s.Kind)
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "path", value: s.Path},
		{name: "url", value: s.URL},
		{name: "ref", value: s.Ref},
	} {
		if field.value != "" && strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("%s must not contain leading or trailing whitespace", field.name)
		}
	}
	return nil
}

func stageEnvironmentAllows(name string, extra []string) bool {
	for _, allowed := range procenv.Vars {
		if strings.EqualFold(name, allowed) {
			return true
		}
	}
	for _, allowed := range extra {
		if strings.EqualFold(name, allowed) {
			return true
		}
	}
	for _, prefix := range procenv.Prefixes {
		if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func validateOTLPEndpoint(endpoint string, insecure bool) error {
	var host, scheme string
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("must be a valid URL: %w", err)
		}
		scheme = strings.ToLower(u.Scheme)
		if scheme != "https" && scheme != "http" {
			return fmt.Errorf("scheme must be https, or http with insecure mode")
		}
		if u.Host == "" || u.Hostname() == "" {
			return fmt.Errorf("host is required")
		}
		if strings.HasSuffix(u.Host, ":") {
			return fmt.Errorf("port must not be empty")
		}
		if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("userinfo, paths, queries, and fragments are not supported")
		}
		host = u.Hostname()
		if port := u.Port(); port != "" {
			if err := validateCollectorPort(port); err != nil {
				return err
			}
		}
	} else {
		if strings.ContainsAny(endpoint, "/?#@") {
			return fmt.Errorf("must be a host:port address or an http(s) URL")
		}
		var port string
		var err error
		host, port, err = net.SplitHostPort(endpoint)
		if err != nil {
			return fmt.Errorf("must be a host:port address: %w", err)
		}
		if host == "" {
			return fmt.Errorf("host is required")
		}
		if err := validateCollectorPort(port); err != nil {
			return err
		}
	}

	if scheme == "http" && !insecure {
		return fmt.Errorf("http requires explicit insecure: true")
	}
	if scheme == "https" && insecure {
		return fmt.Errorf("https conflicts with insecure: true")
	}
	if insecure && !isLoopbackHost(host) {
		return fmt.Errorf("insecure mode is allowed only for localhost or a loopback IP")
	}
	return nil
}

func validateCollectorPort(port string) error {
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("port %q must be a number from 1 through 65535", port)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHeaderName(name string) bool {
	if name == "" || strings.HasPrefix(strings.ToLower(name), "grpc-") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func validateAPIListenAddress(address string) error {
	return validateLoopbackListenAddress(address)
}

func validateLoopbackListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("must be a host:port address: %w", err)
	}
	if host == "" {
		return fmt.Errorf("host is required; wildcard listeners are not allowed")
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("host %q is not loopback", host)
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("port %q must be a number from 0 through 65535", port)
	}
	return nil
}

// WriteConfig marshals cfg as YAML and writes it to path.
func WriteConfig(path string, cfg *Config) error {
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal instance config: %w", err)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return fmt.Errorf("marshal instance config: %w", err)
	}
	if err := os.WriteFile(path, yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
