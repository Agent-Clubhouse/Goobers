package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/speechnotify"
)

// APIVersion and Kind for instance.yaml. Mirrors the config-as-code
// apiVersion/kind convention (ARCHITECTURE.md §6) though instance.yaml is a
// provisioning file, never a CR the operator reconciles.
const (
	ConfigAPIVersion                 = "goobers.dev/v1alpha1"
	ConfigKind                       = "Instance"
	DefaultAPIListenAddress          = "127.0.0.1:8080"
	DefaultWebhookListenAddress      = "127.0.0.1:8081"
	DefaultTemporalHostPort          = "127.0.0.1:7233"
	DefaultTemporalNamespace         = "default"
	DefaultEngineTaskQueue           = "goobers-engine"
	TemporalHostPortEnv              = "GOOBERS_TEMPORAL_HOSTPORT"
	TemporalAddressEnv               = "GOOBERS_TEMPORAL_ADDRESS"
	TemporalAddressLegacyEnv         = "TEMPORAL_ADDRESS"
	TemporalNamespaceEnv             = "GOOBERS_TEMPORAL_NAMESPACE"
	TemporalNamespaceLegacyEnv       = "TEMPORAL_NAMESPACE"
	TaskQueueEnv                     = "GOOBERS_TASK_QUEUE"
	TemporalTaskQueueEnv             = "GOOBERS_TEMPORAL_TASK_QUEUE"
	TemporalTaskQueueLegacyEnv       = "TEMPORAL_TASK_QUEUE"
	OTLPEndpointEnv                  = "GOOBERS_OTLP_ENDPOINT"
	OTLPInsecureEnv                  = "GOOBERS_OTLP_INSECURE"
	DefaultWorkflowSourceRef         = "main"
	WorkflowSourceKindLocalDir       = "local-dir"
	WorkflowSourceKindGit            = "git"
	DefaultDaemonLivenessTimeout     = 2 * time.Minute
	MinimumDaemonLivenessTimeout     = 2 * time.Second
	DefaultStalledRunTimeout         = runcontrol.DefaultStalledRunTimeout
	DefaultClaimsLockTimeout         = 30 * time.Second
	DefaultTelemetryRetentionWindow  = 90 * 24 * time.Hour
	DefaultTelemetryRetentionMaxRuns = 500
	// LargeRepoDefaultStageTimeout is the preset's deterministic-stage deadline.
	LargeRepoDefaultStageTimeout = "4h"
	// LargeRepoStalledRunTimeout is the preset's journal inactivity watchdog.
	LargeRepoStalledRunTimeout = "6h"
	// LargeRepoMaxRunDuration is the preset's total run-age limit.
	LargeRepoMaxRunDuration = "24h"
)

// Config is the parsed instance.yaml: target repo(s) + provider, token source
// refs, telemetry settings, instance-level run conditions (INST-010), and the
// timezone cron schedules evaluate in (issue #137 — previously promised by
// internal/localscheduler's own doc comments but never actually a field
// anywhere, so every schedule silently ran in whatever the host process's
// local zone happened to be).
type Config struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	// SchemaVersion is the instance-config schema revision (dsl-3.0.md D8,
	// decision record D3) — the config's first version field. Absent means 1,
	// the pre-Goobernetes schema every existing install is on; 2 introduces
	// the runners: inventory. Strict loading on both halves means a
	// schemaVersion-2 config using runners: hard-fails on an older binary by
	// design rather than being silently misread. A pointer so the loader can
	// tell absent from an explicit 0 — the published schema's enum is [1, 2],
	// so an explicit 0 is refused rather than silently read as legacy.
	SchemaVersion *int      `json:"schemaVersion,omitempty" yaml:"schemaVersion,omitempty"`
	Repos         []RepoRef `json:"repos" yaml:"repos"`
	// SelfIdentity is the instance-wide provider login used when a gaggle does
	// not declare its own identity. It is an identity value, not a credential.
	SelfIdentity string `json:"selfIdentity,omitempty" yaml:"selfIdentity,omitempty"`
	// NeedsHumanAssignee is the provider identity assigned to work items when
	// Goobers parks them for human attention.
	NeedsHumanAssignee string `json:"needsHumanAssignee,omitempty" yaml:"needsHumanAssignee,omitempty"`
	// WorkflowSource locates the definitions-as-code tree independently of the
	// target code repositories. Nil keeps the local <instance-root>/config
	// default.
	WorkflowSource *WorkflowSource `json:"workflowSource,omitempty" yaml:"workflowSource,omitempty"`
	API            APIConfig       `json:"api,omitempty" yaml:"api,omitempty"`
	Webhook        WebhookConfig   `json:"webhook,omitempty" yaml:"webhook,omitempty"`
	Portal         PortalConfig    `json:"portal,omitempty" yaml:"portal,omitempty"`
	Telemetry      TelemetryConfig `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	// Engine configures the tier-3 Temporal runner. Nil keeps the local daemon's
	// projection loop disabled; standalone engine commands still use defaults.
	Engine                  *EngineConfig `json:"engine,omitempty" yaml:"engine,omitempty"`
	engineResolutionApplied bool
	engineProjectionEnabled bool
	// ExternalTelemetry declares named, read-only operational telemetry
	// connectors. Workflows select only a connector name and generic query
	// inputs; provider fields remain confined to each connector's config.
	ExternalTelemetry externaltelemetry.Configuration `json:"externalTelemetry,omitempty" yaml:"externalTelemetry,omitempty"`
	RunConditions     RunConditions                   `json:"runConditions,omitempty" yaml:"runConditions,omitempty"`
	Retention         RetentionConfig                 `json:"retention,omitempty" yaml:"retention,omitempty"`
	// Notifications opts `goobers up` into native desktop notifications for
	// escalated and failed runs. It defaults to false.
	Notifications bool `json:"notifications,omitempty" yaml:"notifications,omitempty"`
	// Speech configures an opt-in local speech sink for the same terminal alerts.
	Speech *speechnotify.Config `json:"speech,omitempty" yaml:"speech,omitempty"`
	// Credentials sources individual stage capabilities or named BYO MCP
	// credentials from their own token refs. A capability entry overrides any
	// repo-token default; an MCP entry is reachable only through an explicit
	// goober server reference.
	Credentials []CredentialGrant `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// DaemonIdentity declares a distinct bot identity for the daemon's own
	// authored PRs/reviews/merges/comments (UNOP-7/#1295, #1780), backing the
	// standard daemon-mutation capability set unless a capability has its own
	// Credentials override. Nil (the default) is byte-identical to every
	// instance today.
	DaemonIdentity *DaemonIdentityConfig `json:"daemonIdentity,omitempty" yaml:"daemonIdentity,omitempty"`
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
	// Runners is the plural runner inventory (decision record D3, dsl-3.0.md
	// §3): every runner class the scheduler may place stages on. Absent, the
	// legacy singular Runner block above maps to the implicit "self" entry —
	// the zero-change upgrade every existing install rides (ResolvedRunners).
	// Declared, it owns capability claims: Runner.Capabilities must then be
	// empty (supersession, no coexistence), while Runner's execution settings
	// (envPassthrough, timeouts, harnessCommand) keep their current homes.
	// Inventory edits are restart-only in v1 (accept-and-pin, D9): instance.yaml
	// is startup-only, so in-flight runs finish against their pinned snapshot.
	Runners []RunnerEntry `json:"runners,omitempty" yaml:"runners,omitempty"`
	// SecretStores declares named external secret stores token refs can resolve
	// through (config half of #683, SEC-010). A token ref opts in per ref with
	// store: "<storeName>/<secretName>"; an instance that declares no stores and
	// uses only env/file refs behaves byte-identically to before this field
	// existed.
	SecretStores []SecretStoreConfig `json:"secretStores,omitempty" yaml:"secretStores,omitempty"`
	// Sandbox declares the instance-wide isolation posture (#1305). Absent or
	// zero-valued it is "disabled" — sandboxing is strictly opt-in, so an
	// unconfigured instance runs exactly as before. A gaggle may override it
	// through GaggleSpec.Sandbox (EffectiveAgenticSandbox).
	Sandbox *SandboxConfig `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
	// Workcopies tunes managed working-copy provisioning (docs/design/
	// v2-cloud-scale.md §3). Nil keeps every default — a pointer, unlike the
	// sibling sections, so an unconfigured instance's written instance.yaml
	// stays byte-identical.
	Workcopies *WorkcopiesConfig `json:"workcopies,omitempty" yaml:"workcopies,omitempty"`
}

// WorkcopiesConfig tunes how the worktree manager provisions managed mirrors.
type WorkcopiesConfig struct {
	// Root is an optional absolute base path for managed mirrors and worktrees.
	// Gaggle names are appended beneath it to preserve workforce isolation.
	Root string `json:"root,omitempty" yaml:"root,omitempty"`
	// PartialClone opts newly created mirrors into blobless partial clones
	// with a heads+tags-narrowed refresh refspec (#646, design §3 B1): blobs
	// are fetched on demand when a stage worktree first materializes them,
	// which makes worktree provisioning network-dependent (and, on private
	// repos, credential-dependent — see worktree.WithPartialClone). False —
	// the default — keeps mirrors full clones with git invocations
	// byte-identical to previous releases; existing mirrors are never
	// migrated in either direction.
	PartialClone bool `json:"partialClone,omitempty" yaml:"partialClone,omitempty"`
	// ObjectCache opts newly created mirrors into borrowing objects from a
	// shared, node-level object cache via git alternates (#654, design §3
	// B3): one bare mirror clone per repo URL, shared by every gaggle
	// Manager on the node targeting that repo, instead of each gaggle
	// paying for its own full clone. False — the default — keeps mirror
	// creation byte-identical to previous releases; no `_objects` cache
	// directory is ever created. See worktree.WithObjectCache.
	ObjectCache bool `json:"objectCache,omitempty" yaml:"objectCache,omitempty"`
}

// PartialCloneEnabled reports whether newly created mirrors should be
// blobless partial clones (workcopies.partialClone, defaults to false).
func (c *Config) PartialCloneEnabled() bool {
	return c.Workcopies != nil && c.Workcopies.PartialClone
}

// ObjectCacheEnabled reports whether newly created mirrors should reference
// a shared node-level object cache (workcopies.objectCache, defaults to
// false).
func (c *Config) ObjectCacheEnabled() bool {
	return c.Workcopies != nil && c.Workcopies.ObjectCache
}

// EffectiveWorkcopiesLayout applies the gaggle override, then the instance
// override, to layout. An empty root preserves the instance-local default.
func EffectiveWorkcopiesLayout(layout Layout, c *Config, gaggle *apiv1.Gaggle) (Layout, error) {
	root := ""
	if c != nil && c.Workcopies != nil {
		root = c.Workcopies.Root
	}
	if gaggle != nil && gaggle.Spec.Workcopies != nil && gaggle.Spec.Workcopies.Root != "" {
		root = gaggle.Spec.Workcopies.Root
	}
	if root == "" {
		return layout, nil
	}
	if !filepath.IsAbs(root) {
		return Layout{}, fmt.Errorf("workcopies.root must be an absolute path: %q", root)
	}
	return layout.WithWorkcopiesRoot(filepath.Clean(root)), nil
}

// EffectiveSelfIdentity returns the provider login configured for gaggle,
// falling back to the instance-wide default. Empty means assignment-aware
// backlog selection remains opted out.
func EffectiveSelfIdentity(c *Config, gaggle *apiv1.Gaggle) string {
	if gaggle != nil && gaggle.Spec.SelfIdentity != "" {
		return gaggle.Spec.SelfIdentity
	}
	if c == nil {
		return ""
	}
	return c.SelfIdentity
}

// EffectiveSpeechConfig returns the configured speech settings or disabled
// defaults when the speech section is absent.
func (c *Config) EffectiveSpeechConfig() speechnotify.Config {
	if c.Speech == nil {
		return speechnotify.Config{}
	}
	return *c.Speech
}

// WorkflowSource locates the workflow configuration independently of Repos.
// A local-dir source reads Path directly. A git source reads a committed Ref
// from either a local repository Path or a remote HTTPS URL; remote sources
// authenticate through their own token reference or a github-app auth block
// (#3274) — exactly one of the two.
type WorkflowSource struct {
	Kind  string    `json:"kind" yaml:"kind"`
	Path  string    `json:"path,omitempty" yaml:"path,omitempty"`
	URL   string    `json:"url,omitempty" yaml:"url,omitempty"`
	Ref   string    `json:"ref,omitempty" yaml:"ref,omitempty"`
	Token *TokenRef `json:"token,omitempty" yaml:"token,omitempty"`
	// Auth selects GitHub App installation-token minting for a REMOTE git
	// source (#3274), reusing repos[]' RepoAuthConfig shape (kind github-app
	// with appId/installationId/privateKey) the way DaemonIdentityConfig
	// reuses the Kind vocabulary — the underlying mechanism is identical.
	// Mutually exclusive with Token: exactly one identity mechanism per
	// source, exactly as repos[] treats it. Nil preserves static-token
	// behavior unchanged.
	Auth *RepoAuthConfig `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// GitHubAppAuth reports whether the workflow source authenticates through
// GitHub App installation-token minting (auth kind github-app, #3274) rather
// than a static token ref.
func (s WorkflowSource) GitHubAppAuth() bool {
	return s.Auth != nil && s.Auth.Kind == GitHubAuthApp
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
	// DefaultStageTimeout is the baseline deadline for a deterministic stage
	// that declares no timeoutSeconds of its own. Empty keeps the built-in
	// executor.DefaultTimeout, so an unconfigured instance is unchanged.
	//
	// The deterministic twin of the goober-level harness default (#1070): a
	// stage's own timeoutSeconds has always been declarable, but the value it
	// falls back to was a hardcoded 10 minutes with no lever. That default was
	// sized for short commands and is smaller than a real build+test command on
	// a mature repo — an adopter whose `make ci` takes 15 minutes otherwise has
	// to stamp timeoutSeconds onto every such stage in every workflow. A
	// timed-out stage is reported retryable, but a workflow that routes the
	// failure to an agent instead spends a repass on something no diff can fix
	// (#1969).
	//
	// Per-stage timeoutSeconds still wins; this only moves the floor.
	DefaultStageTimeout string `json:"defaultStageTimeout,omitempty" yaml:"defaultStageTimeout,omitempty"`
	// HarnessCommand overrides the base CLI invocation (argv[0..]) launched for
	// a harness, keyed by harness name ("copilot", "claude-code"). Unset keys
	// keep the built-in default (["copilot"] / ["claude"]).
	//
	// The launcher was always data on the adapter (harness.CopilotAdapter.Command)
	// but hardcoded at the composition root, so pointing a harness at a
	// contract-compatible wrapper required a Goobers code change. This is the
	// adopter escape hatch (the launcher twin of EnvPassthrough, #736): a
	// deployment can run the same engine CLI through a wrapper — e.g.
	// {"copilot": ["agency", "copilot"]} to launch a wrapper that forwards to
	// the GitHub Copilot CLI and emits the same session/result artifacts — with
	// no code change and no new harness. Goobers stays vendor-neutral: the
	// wrapper name lives only in the adopter's instance.yaml, never in the enum.
	//
	// Each value must be a non-empty argv whose first element (the program) is
	// non-empty; keys must be a known harness name. Validated at load, fail
	// closed. Downstream harness logic (model/context/extra-arg selection,
	// session capture, completion-file readback) is unchanged — the override
	// only replaces the launch prefix, so it is safe only for a launcher that
	// honors the same CLI contract as the harness it overrides.
	HarnessCommand map[string][]string `json:"harnessCommand,omitempty" yaml:"harnessCommand,omitempty"`
}

// APIConfig configures the daemon's read-only HTTP API.
type APIConfig struct {
	// Listen is a host:port address. A loopback host keeps the tier-1
	// local-trust posture (SEC-040). A non-loopback host is refused at load
	// time unless both TLS and Auth are configured — fail closed, with no
	// insecure override (#640).
	Listen string `json:"listen,omitempty" yaml:"listen,omitempty"`
	// TLS serves the API over HTTPS from an on-disk certificate/key pair.
	// Required for a non-loopback listen address.
	TLS *APITLSConfig `json:"tls,omitempty" yaml:"tls,omitempty"`
	// Auth replaces the tier-1 null authenticator (SEC-043). Required for a
	// non-loopback listen address.
	Auth *APIAuthConfig `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// APITLSConfig points at the API server's TLS certificate and private key.
// Paths only — key material never appears in instance.yaml (CFG-009).
type APITLSConfig struct {
	CertFile string `json:"certFile" yaml:"certFile"`
	KeyFile  string `json:"keyFile" yaml:"keyFile"`
}

// APIAuthConfig selects the daemon API authenticator behind the
// httpapi.Authenticator seam. OIDC bearer-token validation is the only
// implementation; Entra ID is a configured issuer, not a code path.
type APIAuthConfig struct {
	OIDC *OIDCAuthConfig `json:"oidc,omitempty" yaml:"oidc,omitempty"`
}

// DefaultOIDCRolesClaim is the token claim consulted for role values when
// api.auth.oidc.rolesClaim is not set.
const DefaultOIDCRolesClaim = "roles"

// OIDCAuthConfig validates bearer JWTs against one configured issuer and maps
// issuer claims onto the instance roles (view/operate/admin, #644).
type OIDCAuthConfig struct {
	// Issuer is the OIDC issuer URL, exactly as tokens state it in iss.
	// Discovery uses <issuer>/.well-known/openid-configuration.
	Issuer string `json:"issuer" yaml:"issuer"`
	// Audience is the aud claim value tokens must carry.
	Audience string `json:"audience" yaml:"audience"`
	// RolesClaim names the claim carrying role/group values (e.g. "roles",
	// "groups"). Empty defaults to DefaultOIDCRolesClaim.
	RolesClaim string `json:"rolesClaim,omitempty" yaml:"rolesClaim,omitempty"`
	// Roles maps claim values onto instance roles. Deny by default: an
	// authenticated principal whose claim values match nothing gets no role.
	Roles OIDCRoleMapping `json:"roles" yaml:"roles"`
}

// OIDCRoleMapping lists the issuer claim values granted each instance role.
// Roles are ordered: admin implies operate, operate implies view.
type OIDCRoleMapping struct {
	View    []string `json:"view,omitempty" yaml:"view,omitempty"`
	Operate []string `json:"operate,omitempty" yaml:"operate,omitempty"`
	Admin   []string `json:"admin,omitempty" yaml:"admin,omitempty"`
}

// RolesClaimName returns the configured roles claim, defaulting to "roles".
func (c OIDCAuthConfig) RolesClaimName() string {
	if c.RolesClaim == "" {
		return DefaultOIDCRolesClaim
	}
	return c.RolesClaim
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

// PortalConfig holds operator-supplied dashboard co-branding (CBR).
type PortalConfig struct {
	Brand   PortalBrandConfig   `json:"brand,omitempty" yaml:"brand,omitempty"`
	Theme   PortalThemeConfig   `json:"theme,omitempty" yaml:"theme,omitempty"`
	Support PortalSupportConfig `json:"support,omitempty" yaml:"support,omitempty"`
}

// PortalBrandConfig holds the per-instance brand identity (name, tagline,
// scope mark, logo, favicon) surfaced in the portal for co-branding (CBR).
type PortalBrandConfig struct {
	Name       string `json:"name,omitempty" yaml:"name,omitempty"`
	Tagline    string `json:"tagline,omitempty" yaml:"tagline,omitempty"`
	ScopeMark  string `json:"scopeMark,omitempty" yaml:"scopeMark,omitempty"`
	LogoURL    string `json:"logoUrl,omitempty" yaml:"logoUrl,omitempty"`
	FaviconURL string `json:"faviconUrl,omitempty" yaml:"faviconUrl,omitempty"`
}

// PortalThemeConfig holds the per-instance accent color overrides (light and
// dark variants) applied to the portal for co-branding (CBR).
type PortalThemeConfig struct {
	AccentLight     string `json:"accentLight,omitempty" yaml:"accentLight,omitempty"`
	AccentDark      string `json:"accentDark,omitempty" yaml:"accentDark,omitempty"`
	AccentSoftLight string `json:"accentSoftLight,omitempty" yaml:"accentSoftLight,omitempty"`
	AccentSoftDark  string `json:"accentSoftDark,omitempty" yaml:"accentSoftDark,omitempty"`
	AccentInkLight  string `json:"accentInkLight,omitempty" yaml:"accentInkLight,omitempty"`
	AccentInkDark   string `json:"accentInkDark,omitempty" yaml:"accentInkDark,omitempty"`
}

// PortalSupportConfig holds the per-instance support channels (docs, issues,
// chat, and extra links) rendered in the portal sidebar for co-branding (CBR).
type PortalSupportConfig struct {
	DocsURL   string              `json:"docsUrl,omitempty" yaml:"docsUrl,omitempty"`
	IssuesURL string              `json:"issuesUrl,omitempty" yaml:"issuesUrl,omitempty"`
	ChatURL   string              `json:"chatUrl,omitempty" yaml:"chatUrl,omitempty"`
	Links     []PortalSupportLink `json:"links,omitempty" yaml:"links,omitempty"`
}

// PortalSupportLink is a single labeled support URL shown in the portal's
// support footer (CBR).
type PortalSupportLink struct {
	Label string `json:"label" yaml:"label"`
	URL   string `json:"url" yaml:"url"`
}

// RepoRef is a target repository this instance connects to.
type RepoRef struct {
	// Provider is the backing system: "github", "ado", or "gitea".
	Provider string `json:"provider" yaml:"provider"`
	// BaseURL is the forge root URL (e.g. https://gitea.example.com). Required
	// when provider=gitea so stage subprocesses can resolve the self-hosted
	// host from config; omitted for github/ado.
	BaseURL string `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
	// Owner is the GitHub owner or Azure DevOps organization.
	Owner string `json:"owner" yaml:"owner"`
	// Project is required for Azure DevOps and omitted for GitHub.
	Project string `json:"project,omitempty" yaml:"project,omitempty"`
	// Name is the repo name.
	Name string `json:"name" yaml:"name"`
	// LargeRepo enables the monolith-safe execution preset. The preset supplies
	// defaults only; explicit repository workspace, path-length, stage-timeout,
	// and run-control settings override it.
	LargeRepo bool `json:"largeRepo,omitempty" yaml:"largeRepo,omitempty"`
	// Token is a reference to this repo's credential. Never an inline value
	// (CFG-009, SEC-010). GitHub and ADO PAT auth require exactly one of Env
	// or File. Entra-backed ADO auth and GitHub App auth do not use this
	// field — exactly one identity mechanism per repo.
	Token TokenRef `json:"token,omitempty" yaml:"token,omitempty"`
	// Auth selects a non-default credential source for this repo: an Azure
	// DevOps identity kind, or GitHub App installation-token minting (#686).
	// Nil preserves PAT behavior with Token configured.
	Auth *RepoAuthConfig `json:"auth,omitempty" yaml:"auth,omitempty"`
	// Policy declares this repo's forge-conformance manifest (issue #916,
	// Tier 4 of #903): the live GitHub settings `goobers doctor --repo`
	// checks against. Nil declares no expectation, so an instance that
	// configures none behaves exactly as before.
	// +optional
	Policy *RepoPolicyExpectation `json:"policy,omitempty" yaml:"policy,omitempty"`
	// PathLength configures the checkout path-length preflight for this repo.
	// On Windows the preflight defaults to the 260-character MAX_PATH ceiling;
	// declaring this block enables it on every host. Set disabled to opt out.
	// +optional
	PathLength *RepoPathLengthConfig `json:"pathLength,omitempty" yaml:"pathLength,omitempty"`
	// Workspace selects how this repository is materialized for local runs.
	// Pinned mode is intentionally non-hermetic: ignored and untracked build
	// state may persist between runs, so the target repository's .gitignore
	// hygiene is load-bearing for clean run-branch diffs.
	Workspace *RepoWorkspaceConfig `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	// DefaultStageTimeout is the deadline for deterministic stages that omit
	// timeoutSeconds. It overrides both the large-repo preset and the
	// instance-wide runner.defaultStageTimeout for this repository.
	DefaultStageTimeout string `json:"defaultStageTimeout,omitempty" yaml:"defaultStageTimeout,omitempty"`
	// RunControls overrides instance-level watchdog defaults for runs targeting
	// this repository. Gaggle and workflow runControls remain more specific.
	RunControls *apiv1.RunControls `json:"runControls,omitempty" yaml:"runControls,omitempty"`
}

// RepoPathLengthConfig bounds paths a repository checkout and its build output
// may create beneath a managed worktree.
type RepoPathLengthConfig struct {
	// Disabled explicitly opts this repository out of path-length preflight.
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	// MaxPathLength is the absolute path ceiling. Zero defaults to 260.
	MaxPathLength int `json:"maxPathLength,omitempty" yaml:"maxPathLength,omitempty"`
	// BuildOutputAllowance reserves characters beyond the deepest tracked path
	// for build-generated subdirectories and files.
	BuildOutputAllowance int `json:"buildOutputAllowance,omitempty" yaml:"buildOutputAllowance,omitempty"`
}

const (
	// WorkspaceCleanNone preserves ignored and untracked files between runs.
	WorkspaceCleanNone = "none"
	// WorkspaceCleanIgnoredSafe removes untracked files while preserving ignored files.
	WorkspaceCleanIgnoredSafe = "ignored-safe"
	// WorkspaceCleanFull removes all ignored and untracked files.
	WorkspaceCleanFull = "full"
)

// RepoWorkspaceConfig configures the mutually exclusive local checkout modes.
// Worktrees is explicit only so contradictory declarations fail loudly;
// omitting Workspace retains the existing per-stage worktree behavior.
type RepoWorkspaceConfig struct {
	Pinned      bool   `json:"pinned" yaml:"pinned"`
	Worktrees   bool   `json:"worktrees,omitempty" yaml:"worktrees,omitempty"`
	CleanPolicy string `json:"cleanPolicy,omitempty" yaml:"cleanPolicy,omitempty"`
	pinnedSet   bool
}

// UnmarshalJSON preserves whether pinned was explicitly declared so false can
// override the large-repo preset rather than looking identical to omission.
func (c *RepoWorkspaceConfig) UnmarshalJSON(data []byte) error {
	var wire struct {
		Pinned      *bool  `json:"pinned"`
		Worktrees   bool   `json:"worktrees"`
		CleanPolicy string `json:"cleanPolicy"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	c.Worktrees = wire.Worktrees
	c.CleanPolicy = wire.CleanPolicy
	c.pinnedSet = wire.Pinned != nil
	if wire.Pinned != nil {
		c.Pinned = *wire.Pinned
	}
	return nil
}

// Pinned reports whether this repository uses its node-local persistent copy.
func (r RepoRef) Pinned() bool {
	if r.Workspace != nil {
		if r.Workspace.Worktrees {
			return false
		}
		if r.Workspace.pinnedSet {
			return r.Workspace.Pinned
		}
		if r.Workspace.Pinned {
			return true
		}
	}
	return r.LargeRepo
}

// WorkspaceCleanPolicy returns the configured pinned clean policy, defaulting
// to none so ignored and untracked incremental build state survives.
func (r RepoRef) WorkspaceCleanPolicy() string {
	if r.Workspace == nil || r.Workspace.CleanPolicy == "" {
		return WorkspaceCleanNone
	}
	return r.Workspace.CleanPolicy
}

// EffectiveDefaultStageTimeout returns the repository-specific deterministic
// stage default after applying the large-repo preset and instance fallback.
func (r RepoRef) EffectiveDefaultStageTimeout(fallback string) string {
	if r.DefaultStageTimeout != "" {
		return r.DefaultStageTimeout
	}
	if r.LargeRepo {
		return LargeRepoDefaultStageTimeout
	}
	return fallback
}

// EffectiveRunControls overlays repository defaults on instance run controls.
// Gaggle and workflow controls are applied by runcontrol.Resolve afterwards.
func (r RepoRef) EffectiveRunControls(base apiv1.RunControls) apiv1.RunControls {
	if r.LargeRepo {
		base.StalledRunTimeout = LargeRepoStalledRunTimeout
		base.MaxRunDuration = LargeRepoMaxRunDuration
	}
	if r.RunControls == nil {
		return base
	}
	if r.RunControls.MaxRepasses > 0 {
		base.MaxRepasses = r.RunControls.MaxRepasses
	}
	if r.RunControls.StalledRunTimeout != "" {
		base.StalledRunTimeout = r.RunControls.StalledRunTimeout
	}
	if r.RunControls.MaxRunDuration != "" {
		base.MaxRunDuration = r.RunControls.MaxRunDuration
	}
	return base
}

// ResolveLargeRepoPresets materializes monolith-safe defaults so config
// inspection and every runtime consumer see the same effective values.
func (c *Config) ResolveLargeRepoPresets() {
	for i := range c.Repos {
		repo := &c.Repos[i]
		if !repo.LargeRepo {
			continue
		}
		if repo.Workspace == nil {
			repo.Workspace = &RepoWorkspaceConfig{Pinned: true}
		} else if !repo.Workspace.Worktrees && !repo.Workspace.pinnedSet {
			repo.Workspace.Pinned = true
		}
		if repo.PathLength == nil {
			repo.PathLength = &RepoPathLengthConfig{}
		}
		if repo.DefaultStageTimeout == "" {
			repo.DefaultStageTimeout = LargeRepoDefaultStageTimeout
		}
		if repo.RunControls == nil {
			repo.RunControls = &apiv1.RunControls{}
		}
		if repo.RunControls.StalledRunTimeout == "" {
			repo.RunControls.StalledRunTimeout = LargeRepoStalledRunTimeout
		}
		if repo.RunControls.MaxRunDuration == "" {
			repo.RunControls.MaxRunDuration = LargeRepoMaxRunDuration
		}
	}
}

// RepoPolicyExpectation is one repo's declared forge-conformance manifest
// (issue #916): the settings `goobers doctor --repo` diffs against the
// repo's live GitHub state. GitHub-only in V1 — no ADO equivalent is
// modeled, per the curated V1 contract. Declared at the instance-config
// level (this file) rather than the product repo, since these are
// deployment/ops-owned rulesets, not workflow logic.
type RepoPolicyExpectation struct {
	// Branch is the ruleset/branch-protection target branch these
	// expectations apply to. Empty defaults to "main".
	// +optional
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`
	// RequiredMergeMethod is the one merge method the repo's live settings
	// must allow exclusively — "merge", "squash", or "rebase" (the
	// squash-only-ruleset-vs-merge-commit scenario, #877). Empty imposes no
	// requirement.
	// +optional
	// +kubebuilder:validation:Enum=merge;squash;rebase
	RequiredMergeMethod string `json:"requiredMergeMethod,omitempty" yaml:"requiredMergeMethod,omitempty"`
	// MergeQueueRequired declares that Branch must require GitHub's native
	// merge queue (a "merge_queue"-typed branch ruleset rule) rather than
	// accepting a direct-merge path (#882).
	// +optional
	MergeQueueRequired bool `json:"mergeQueueRequired,omitempty" yaml:"mergeQueueRequired,omitempty"`
	// RequiredStatusChecks lists the check contexts Branch's live rules must
	// require. Empty imposes no requirement.
	// +optional
	RequiredStatusChecks []string `json:"requiredStatusChecks,omitempty" yaml:"requiredStatusChecks,omitempty"`
}

// GitHubAppAuth reports whether this repo authenticates through GitHub App
// installation-token minting (auth kind github-app, #686) rather than a
// static token ref.
func (r RepoRef) GitHubAppAuth() bool {
	return r.Provider == "github" && r.Auth != nil && r.Auth.Kind == GitHubAuthApp
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

const (
	// GitHubAuthPAT selects the env/file/Keychain/store-backed static token — the
	// default when auth is absent, byte-identical to before GitHub repos
	// accepted an auth block at all.
	GitHubAuthPAT = "pat"
	// GitHubAuthApp selects GitHub App installation-token minting (#686):
	// short-lived, installation-scoped tokens exchanged for a signed App JWT
	// per resolve, replacing a static PAT with no rotation machinery.
	GitHubAuthApp = "github-app"
)

// RepoAuthConfig selects a repository credential source without embedding
// credential material in configuration. Kind values are provider-specific:
// ADO accepts pat/azure-cli/workload-identity/managed-identity, GitHub
// accepts pat/github-app; fields beyond Kind belong to one provider's kinds
// and are rejected elsewhere at load.
type RepoAuthConfig struct {
	Kind string `json:"kind" yaml:"kind"`
	// Tenant optionally pins Azure CLI authentication to one tenant (ADO).
	Tenant string `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	// ClientID optionally selects a user-assigned managed identity (ADO).
	ClientID string `json:"clientId,omitempty" yaml:"clientId,omitempty"`
	// AppID identifies the GitHub App for kind github-app: the numeric App
	// ID or the app's client ID string — GitHub accepts either as the App
	// JWT issuer.
	AppID GitHubID `json:"appId,omitempty" yaml:"appId,omitempty"`
	// InstallationID is the numeric ID of the App's installation on the
	// target repo's owner (kind github-app).
	InstallationID GitHubID `json:"installationId,omitempty" yaml:"installationId,omitempty"`
	// PrivateKey references the App's PEM-encoded private key for kind
	// github-app — env, file, Keychain, or store, exactly like a token ref; never an
	// inline value (CFG-009). The key only ever signs short-lived App JWTs
	// in-process; stages receive minted installation tokens, never the key.
	PrivateKey *TokenRef `json:"privateKey,omitempty" yaml:"privateKey,omitempty"`
	// Slug is the App's URL-safe handle (the part before "[bot]" in its
	// GitHub login, e.g. "my-app" for "my-app[bot]") for kind github-app.
	// Installation tokens cannot call GET /user, so the provider identity's
	// login — which every trusted-comment check (claim markers, verdicts,
	// handoffs) compares against — must be declared here (#3343). Without it
	// those checks fail with "Resource not accessible by integration" the
	// first time they run under App auth.
	Slug string `json:"slug,omitempty" yaml:"slug,omitempty"`
}

// BotLogin returns the GitHub login this auth block authenticates as, when
// declarable: the App slug plus "[bot]" for kind github-app with Slug set,
// otherwise empty (a PAT's login is discoverable via GET /user at runtime and
// needs no declaration).
func (a *RepoAuthConfig) BotLogin() string {
	if a == nil || a.Kind != GitHubAuthApp || strings.TrimSpace(a.Slug) == "" {
		return ""
	}
	return strings.TrimSpace(a.Slug) + "[bot]"
}

// hasGitHubAppFields reports whether any github-app-only field is set, for
// fail-closed rejection on kinds that must not carry them.
func (a *RepoAuthConfig) hasGitHubAppFields() bool {
	return a.AppID != "" || a.InstallationID != "" || a.PrivateKey != nil || a.Slug != ""
}

// GitHubID is a GitHub identifier config field YAML authors may write as a
// number (`appId: 123456`) or a string (`appId: "Iv1.…"`); both parse,
// normalized to the string form the GitHub API consumes.
type GitHubID string

// UnmarshalJSON accepts a JSON string or number. sigs.k8s.io/yaml routes
// YAML values through JSON, so this is the single decode path.
func (id *GitHubID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*id = GitHubID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("must be a string or a number, got %s", data)
	}
	*id = GitHubID(n.String())
	return nil
}

// DaemonIdentityConfig declares a distinct bot identity for the daemon's own
// authored mutations (UNOP-7/#1295), on equal footing whichever Kind is
// chosen — a machine-account PAT (#1780) or GitHub App installation minting
// (#1779), reusing the same Kind vocabulary as RepoAuthConfig (GitHubAuthPAT/
// GitHubAuthApp) since the underlying mechanisms are identical. Kind is a
// deliberately open string, not a closed enum (no kubebuilder Enum marker):
// a future third method (e.g. OIDC federation) is an additive Kind value and
// new kind-specific fields on this same struct, never a schema break for the
// first two.
//
// Nil (the default) is byte-identical to every instance today: capabilities
// resolve exactly as they do without this field, and PR/attribution checks
// that consult it fall back unchanged to their pre-#1780 behavior (see
// cmd/goobers/prselect.go's isOwnPullRequest). Configuring it is what makes a
// distinct identity's credential back the standard daemon-mutation
// capability set (repo:push, github:issues:write, github:pr:write,
// github:pr:review, github:branch:delete, github:pr:merge) without having to
// declare each one individually via credentials: — an explicit
// CredentialGrant for one of those capabilities still overrides this.
type DaemonIdentityConfig struct {
	Kind string `json:"kind" yaml:"kind"`
	// Token references the machine account's PAT for kind "pat" — exactly
	// one of env/file/keychain/store, never an inline value (CFG-009).
	Token *TokenRef `json:"token,omitempty" yaml:"token,omitempty"`
	// AppID identifies the GitHub App for kind "github-app" (see
	// RepoAuthConfig.AppID).
	AppID GitHubID `json:"appId,omitempty" yaml:"appId,omitempty"`
	// InstallationID is the App's installation ID for kind "github-app".
	// Mutually exclusive with Installations: one App installation belongs to
	// exactly one owner, so this form only covers a single-owner instance.
	InstallationID GitHubID `json:"installationId,omitempty" yaml:"installationId,omitempty"`
	// Installations binds one installation per owner for kind "github-app"
	// (#3415), so one App, one key, and one slug can serve an instance whose
	// repos span several owners. Mutually exclusive with InstallationID.
	//
	// This exists because the single-installation form is not merely limited,
	// it is runtime-fatal on a multi-owner instance: the daemon identity backs
	// the whole daemon-mutation capability set instance-wide, so a token minted
	// from one owner's installation fails with a 422 the first time a stage
	// touches a repo in another owner. Observed in production, worked around by
	// removing the daemon identity entirely and giving up explicit PR
	// attribution.
	Installations []DaemonInstallation `json:"installations,omitempty" yaml:"installations,omitempty"`
	// PrivateKey references the App's PEM-encoded private key for kind
	// "github-app" (see RepoAuthConfig.PrivateKey).
	PrivateKey *TokenRef `json:"privateKey,omitempty" yaml:"privateKey,omitempty"`
	// Slug is the App's URL-safe handle (the part before "[bot]" in its
	// GitHub login, e.g. "my-app" for "my-app[bot]") for kind "github-app".
	// Installation tokens cannot self-report a login via the REST API the
	// way a PAT can (there is no equivalent of GET /user), so attribution
	// checks need it declared explicitly. Optional and currently
	// forward-compatible only: #1779 (the App path itself) is not yet
	// implemented, so a kind "github-app" daemon identity without Slug set
	// still mints and authenticates correctly, it just cannot yet be
	// distinguished from the branch-prefix heuristic in PR attribution.
	Slug string `json:"slug,omitempty" yaml:"slug,omitempty"`
}

// DaemonInstallation binds one GitHub App installation to the owner it was
// installed on (#3415).
type DaemonInstallation struct {
	// Owner is the GitHub owner this installation covers, matching a
	// repos[].owner value.
	Owner string `json:"owner" yaml:"owner"`
	// InstallationID is the App's installation ID on that owner.
	InstallationID GitHubID `json:"installationId" yaml:"installationId"`
}

// hasGitHubAppFields reports whether any github-app-only field is set, for
// fail-closed rejection on kinds that must not carry them.
func (d *DaemonIdentityConfig) hasGitHubAppFields() bool {
	return d.AppID != "" || d.InstallationID != "" || len(d.Installations) > 0 ||
		d.PrivateKey != nil || d.Slug != ""
}

// InstallationForOwner resolves the installation this identity should mint with
// when acting on owner. It answers for both forms: the single-installation
// form covers whatever owner it was installed on (the caller has already been
// validated as single-owner, so any owner resolves to it), and the per-owner
// form matches by name.
//
// The owner is known where credentials are wired — buildCredentials receives
// the gaggle's owner and builds one resolver per gaggle — so selection happens
// there rather than threading a repo through credentials.ResolveFunc, which
// takes only a context.
func (d *DaemonIdentityConfig) InstallationForOwner(owner string) (GitHubID, bool) {
	if d == nil {
		return "", false
	}
	if len(d.Installations) == 0 {
		return d.InstallationID, d.InstallationID != ""
	}
	for _, binding := range d.Installations {
		if binding.Owner == owner {
			return binding.InstallationID, binding.InstallationID != ""
		}
	}
	return "", false
}

// GitHubApp reports whether this identity authenticates through GitHub App
// installation-token minting rather than a static token ref.
func (d *DaemonIdentityConfig) GitHubApp() bool {
	return d != nil && d.Kind == GitHubAuthApp
}

// validate enforces exactly-one-kind and kind-specific required fields,
// mirroring RepoAuthConfig's per-repo validation switch (same fail-closed
// discipline, same CFG-009/SEC-010 inline-secret rejection).
func (d *DaemonIdentityConfig) validate(envPassthrough []string, stores map[string]bool) error {
	switch d.Kind {
	case GitHubAuthPAT:
		if d.hasGitHubAppFields() {
			return fmt.Errorf("appId, installationId, privateKey, and slug are only valid for kind %q", GitHubAuthApp)
		}
		if d.Token == nil || d.Token.sourceCount() != 1 {
			return fmt.Errorf("token must reference exactly one of env, file, keychain, or store — " +
				"inline secret values are never permitted (CFG-009, SEC-010)")
		}
		if err := validateStoreRef("token", *d.Token, stores); err != nil {
			return err
		}
		if d.Token.Env != "" && stageEnvironmentAllows(d.Token.Env, envPassthrough) {
			return fmt.Errorf(
				"token.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
				d.Token.Env,
			)
		}
	case GitHubAuthApp:
		if d.Token != nil {
			return fmt.Errorf("kind %q must not configure token — the installation token is minted", GitHubAuthApp)
		}
		if d.AppID == "" {
			return fmt.Errorf("appId is required for kind %q", GitHubAuthApp)
		}
		// #3415: exactly one of the two forms. Accepting both would leave the
		// precedence question to whoever reads the code next, and the two
		// answers differ in which owner gets minted for.
		if d.InstallationID == "" && len(d.Installations) == 0 {
			return fmt.Errorf("installationId or installations is required for kind %q", GitHubAuthApp)
		}
		if d.InstallationID != "" && len(d.Installations) > 0 {
			return fmt.Errorf("set either installationId or installations for kind %q, not both — "+
				"installations already carries the per-owner binding", GitHubAuthApp)
		}
		if d.InstallationID != "" {
			if _, err := strconv.ParseUint(string(d.InstallationID), 10, 64); err != nil {
				return fmt.Errorf("installationId %q must be the numeric installation ID", d.InstallationID)
			}
		}
		seenOwners := make(map[string]bool, len(d.Installations))
		for i, binding := range d.Installations {
			if binding.Owner == "" {
				return fmt.Errorf("installations[%d]: owner is required", i)
			}
			if seenOwners[binding.Owner] {
				return fmt.Errorf("installations[%d]: owner %q is bound more than once — "+
					"GitHub allows one installation per App per owner", i, binding.Owner)
			}
			seenOwners[binding.Owner] = true
			if binding.InstallationID == "" {
				return fmt.Errorf("installations[%d] (%s): installationId is required", i, binding.Owner)
			}
			if _, err := strconv.ParseUint(string(binding.InstallationID), 10, 64); err != nil {
				return fmt.Errorf("installations[%d] (%s): installationId %q must be the numeric installation ID",
					i, binding.Owner, binding.InstallationID)
			}
		}
		if d.PrivateKey == nil || d.PrivateKey.sourceCount() != 1 {
			return fmt.Errorf("privateKey must reference exactly one of env, file, keychain, or store — " +
				"inline secret values are never permitted (CFG-009, SEC-010)")
		}
		if err := validateStoreRef("privateKey", *d.PrivateKey, stores); err != nil {
			return err
		}
		// The App key can mint tokens broadly — never allow the stage
		// environment to carry it, mirroring RepoAuthConfig's own guard.
		if d.PrivateKey.Env != "" && stageEnvironmentAllows(d.PrivateKey.Env, envPassthrough) {
			return fmt.Errorf(
				"privateKey.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
				d.PrivateKey.Env,
			)
		}
	default:
		return fmt.Errorf("unsupported kind %q (supported: %q, %q)", d.Kind, GitHubAuthPAT, GitHubAuthApp)
	}
	return nil
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
// variable name, a path to a file containing it, a macOS Keychain service
// name, or a secret in a declared external secret store (#683). Exactly one
// source per ref.
type TokenRef struct {
	// Env is the name of an environment variable holding the token.
	Env string `json:"env,omitempty" yaml:"env,omitempty"`
	// File is a path to a file whose contents are the token.
	File string `json:"file,omitempty" yaml:"file,omitempty"`
	// Keychain is the service name of a generic-password item in the macOS
	// login keychain.
	Keychain string `json:"keychain,omitempty" yaml:"keychain,omitempty"`
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
	if r.Keychain != "" {
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

// CredentialTokenRef converts this ref into the credentials package's source
// shape under the given resolver ref name, carrying whichever single source
// (env, file, Keychain, or store) is configured. A store-backed ref resolves only
// through a resolver built with the instance's secret-store registry
// (credentials.NewResolverWithStores); plain credentials.NewResolver fails
// closed on it at construction, so a composition site that was never wired
// for stores rejects the ref with a diagnostic instead of silently reading
// it as unconfigured.
func (r TokenRef) CredentialTokenRef(name string) credentials.TokenRef {
	return credentials.TokenRef{Name: name, Env: r.Env, File: r.File, Keychain: r.Keychain, Store: r.Store}
}

// CredentialGrant sources either one stage capability or one named BYO MCP
// credential from its own token ref. Runner-owned capabilities use their
// dedicated config surfaces instead.
type CredentialGrant struct {
	// Capability is the canonical capability string (internal/capability) this
	// token backs, e.g. "agent:model" or "repo:push" (to override the default).
	Capability string `json:"capability,omitempty" yaml:"capability,omitempty"`
	// MCP names a BYO MCP credential. It is not a stage capability and is
	// reachable only by goobers whose MCP server declarations reference it.
	MCP string `json:"mcp,omitempty" yaml:"mcp,omitempty"`
	// Token is the source of the credential — exactly one supported TokenRef
	// source, like a repo's token; inline secret values are never permitted.
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

// EngineConfig identifies the Temporal frontend and task queue shared by all
// tier-3 engine processes.
type EngineConfig struct {
	HostPort  string `json:"hostPort,omitempty" yaml:"hostPort,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	TaskQueue string `json:"taskQueue,omitempty" yaml:"taskQueue,omitempty"`
}

// RunConditions are instance-level run conditions (§7): max parallel runs and
// per-workflow run budgets.
type RunConditions struct {
	// MaxParallelRuns caps total concurrent runs across every workflow in the
	// instance (internal/localscheduler.Conditions.instanceMaxParallel).
	// Zero or omitted means UNLIMITED — bounded only by each workflow's own
	// MaxConcurrentRuns/MaxRunsPerHour. This is the opposite convention from
	// a workflow's own spec.readiness.maxRunsPerHour, where zero/omitted
	// falls back to a default of 10 rather than meaning unlimited (#3360).
	MaxParallelRuns int            `json:"maxParallelRuns,omitempty" yaml:"maxParallelRuns,omitempty"`
	WorkflowBudgets map[string]int `json:"workflowBudgets,omitempty" yaml:"workflowBudgets,omitempty"`
	// WorkflowDailyBudgets overrides a named workflow's runs-per-day budget
	// (#340), mirroring WorkflowBudgets' per-hour override.
	WorkflowDailyBudgets map[string]int `json:"workflowDailyBudgets,omitempty" yaml:"workflowDailyBudgets,omitempty"`
	// MaxRepasses is the instance default for consecutive non-pass gate
	// evaluations before escalation. Zero uses the built-in default.
	MaxRepasses int32 `json:"maxRepasses,omitempty" yaml:"maxRepasses,omitempty"`
	// StalledRunTimeout is the maximum period a running journal may remain
	// silent before the daemon escalates it. Empty defaults to 45 minutes.
	StalledRunTimeout string `json:"stalledRunTimeout,omitempty" yaml:"stalledRunTimeout,omitempty"`
	// MaxRunDuration is the maximum total wall-clock age of a run. Empty
	// disables the limit.
	MaxRunDuration string `json:"maxRunDuration,omitempty" yaml:"maxRunDuration,omitempty"`
	// ClaimsLockTimeout bounds cross-process claim-ledger lock acquisition.
	// Empty defaults to 30 seconds.
	ClaimsLockTimeout string `json:"claimsLockTimeout,omitempty" yaml:"claimsLockTimeout,omitempty"`
}

// RunControls returns the instance layer of the run-control hierarchy.
func (c RunConditions) RunControls() apiv1.RunControls {
	return apiv1.RunControls{
		MaxRepasses:       c.MaxRepasses,
		StalledRunTimeout: c.StalledRunTimeout,
		MaxRunDuration:    c.MaxRunDuration,
	}
}

// RetentionConfig controls opt-in pruning of retained failure worktrees and
// merged local run branches. Both Enabled and DryRun default to false.
type RetentionConfig struct {
	Enabled                  bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	DryRun                   bool   `json:"dryRun,omitempty" yaml:"dryRun,omitempty"`
	MaxRetainedWorktreeBytes int64  `json:"maxRetainedWorktreeBytes,omitempty" yaml:"maxRetainedWorktreeBytes,omitempty"`
	RetainedWorktreeMaxAge   string `json:"retainedWorktreeMaxAge,omitempty" yaml:"retainedWorktreeMaxAge,omitempty"`
	// ProjectionFullFidelityDays bounds how much history stays INDIVIDUALLY
	// LISTABLE in the portal read model (#1932, §11.4).
	//
	// Independent of journal retention above, and deliberately so: a journal is
	// the source of truth and its retention is a decision about disk and audit;
	// the projection is derived, and this is a decision about what stays
	// listable. Aging a run out of the projection removes no evidence.
	//
	// Beyond the window a run stays answerable IN AGGREGATE but may not be
	// individually listable. That is strictly less than the portal offers
	// today, and was a product decision rather than an engineering one.
	//
	// **0, unset, or negative means UNBOUNDED** — no run is ever aged out. Not
	// "a zero-day window": compared naively that would age out every run
	// immediately, which is the most destructive possible reading of the value
	// an operator would most reasonably expect to mean "off". See
	// readmodel.RetentionDays, where the distinction is enforced rather than
	// documented.
	ProjectionFullFidelityDays int `json:"projectionFullFidelityDays,omitempty" yaml:"projectionFullFidelityDays,omitempty"`
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

// MaxRunDurationDuration resolves the optional total run-age limit.
func (c RunConditions) MaxRunDurationDuration() (time.Duration, error) {
	if c.MaxRunDuration == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(c.MaxRunDuration)
	if err != nil {
		return 0, fmt.Errorf("runConditions.maxRunDuration %q: %w", c.MaxRunDuration, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("runConditions.maxRunDuration must be positive, got %s", duration)
	}
	return duration, nil
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

// DefaultStageTimeoutDuration resolves the baseline deterministic-stage
// deadline. Zero means "unset" — the caller keeps its own built-in default
// rather than substituting one here, so the fallback stays owned by the
// executor that applies it.
func (c RunnerConfig) DefaultStageTimeoutDuration() (time.Duration, error) {
	if c.DefaultStageTimeout == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(c.DefaultStageTimeout)
	if err != nil {
		return 0, fmt.Errorf("runner.defaultStageTimeout %q: %w", c.DefaultStageTimeout, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("runner.defaultStageTimeout must be positive, got %s", timeout)
	}
	return timeout, nil
}

// knownHarnessNames lists the harness names an adopter may key a launcher
// override under, sorted for a stable admission-error message.
func knownHarnessNames() []string {
	return []string{string(apiv1.HarnessClaudeCode), string(apiv1.HarnessCopilot)}
}

// knownHarnessName reports whether name is a harness a launcher override may
// target — the enum's authoritative membership, so a typo fails closed at load
// instead of silently doing nothing.
func knownHarnessName(name string) bool {
	switch apiv1.Harness(name) {
	case apiv1.HarnessCopilot, apiv1.HarnessClaudeCode:
		return true
	default:
		return false
	}
}

// TelemetryEnabled reports whether the local rollup store is enabled
// (defaults to true when unset). Wired into cmd/goobers' up.go/run.go (issue
// #129): telemetry.enabled was documented and set in the real self-hosting
// config (reference-workflows/instance.yaml.example) but had zero callers.
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

// ResolveEngineConfig applies process environment overrides to instance.yaml
// and validates the resulting Temporal connection configuration.
func (c *Config) ResolveEngineConfig(lookupEnv func(string) (string, bool)) (EngineConfig, bool, error) {
	resolved, env, err := c.resolveEngineConfig(lookupEnv)
	return resolved, c.Engine != nil || env.anyOverride, err
}

type engineEnvResolution struct {
	anyOverride  bool
	hostOverride bool
}

func (c *Config) resolveEngineConfig(lookupEnv func(string) (string, bool)) (EngineConfig, engineEnvResolution, error) {
	resolved := EngineConfig{
		HostPort:  DefaultTemporalHostPort,
		Namespace: DefaultTemporalNamespace,
		TaskQueue: DefaultEngineTaskQueue,
	}
	if c.Engine != nil {
		if c.Engine.HostPort != "" {
			resolved.HostPort = c.Engine.HostPort
		}
		if c.Engine.Namespace != "" {
			resolved.Namespace = c.Engine.Namespace
		}
		if c.Engine.TaskQueue != "" {
			resolved.TaskQueue = c.Engine.TaskQueue
		}
	}
	var envResolution engineEnvResolution
	overrides := []struct {
		keys   []string
		target *string
		host   bool
	}{
		{[]string{TemporalHostPortEnv, TemporalAddressEnv, TemporalAddressLegacyEnv}, &resolved.HostPort, true},
		{[]string{TemporalNamespaceEnv, TemporalNamespaceLegacyEnv}, &resolved.Namespace, false},
		{[]string{TaskQueueEnv, TemporalTaskQueueEnv, TemporalTaskQueueLegacyEnv}, &resolved.TaskQueue, false},
	}
	for _, override := range overrides {
		for i, env := range override.keys {
			if value, ok := lookupEnv(env); ok {
				value = strings.TrimSpace(value)
				if value == "" {
					// Compatibility aliases historically used os.Getenv and
					// treated an empty value as unset.
					if i > 0 {
						continue
					}
					return EngineConfig{}, engineEnvResolution{}, fmt.Errorf("%s must not be empty when set", env)
				}
				*override.target = value
				envResolution.anyOverride = true
				envResolution.hostOverride = envResolution.hostOverride || override.host
				break
			}
		}
	}
	if err := resolved.Validate(); err != nil {
		return EngineConfig{}, engineEnvResolution{}, fmt.Errorf("engine: %w", err)
	}
	return resolved, envResolution, nil
}

// EffectiveEngineConfig returns the resolved engine configuration stored by
// LoadConfig, or the standalone defaults when engine is not configured.
func (c *Config) EffectiveEngineConfig() EngineConfig {
	if c.Engine != nil {
		return *c.Engine
	}
	return EngineConfig{
		HostPort:  DefaultTemporalHostPort,
		Namespace: DefaultTemporalNamespace,
		TaskQueue: DefaultEngineTaskQueue,
	}
}

// EngineProjectionEnabled reports whether instance YAML or a host/address
// environment override configured a Temporal connection for the daemon.
func (c *Config) EngineProjectionEnabled() bool {
	if c.engineResolutionApplied {
		return c.engineProjectionEnabled
	}
	return c.Engine != nil
}

// Validate checks the Temporal frontend and task queue fields.
func (c EngineConfig) Validate() error {
	if strings.TrimSpace(c.HostPort) != c.HostPort {
		return fmt.Errorf("hostPort must not contain leading or trailing whitespace")
	}
	host, port, err := net.SplitHostPort(c.HostPort)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return fmt.Errorf("hostPort %q must be in host:port form", c.HostPort)
	}
	if strings.TrimSpace(c.Namespace) != c.Namespace || c.Namespace == "" {
		return fmt.Errorf("namespace must be non-empty without leading or trailing whitespace")
	}
	if strings.TrimSpace(c.TaskQueue) != c.TaskQueue || c.TaskQueue == "" {
		return fmt.Errorf("taskQueue must be non-empty without leading or trailing whitespace")
	}
	return nil
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
			return fmt.Errorf("headers[%q] must reference exactly one of env, file, keychain, or store; inline values are not permitted", name)
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

// EffectivePortalConfig applies built-in dashboard branding defaults.
func (c *Config) EffectivePortalConfig() PortalConfig {
	if c == nil {
		return PortalConfig{
			Brand: PortalBrandConfig{
				Name:      "goobers",
				Tagline:   "local operations",
				ScopeMark: "G",
			},
		}
	}
	effective := c.Portal
	if effective.Brand.Name == "" {
		effective.Brand.Name = "goobers"
	}
	if effective.Brand.Tagline == "" {
		effective.Brand.Tagline = "local operations"
	}
	if effective.Brand.ScopeMark == "" {
		for _, r := range effective.Brand.Name {
			effective.Brand.ScopeMark = string(unicode.ToUpper(r))
			break
		}
	}
	return effective
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
	yamlEngineConfigured := cfg.Engine != nil
	resolvedEngine, engineEnv, err := cfg.resolveEngineConfig(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if yamlEngineConfigured || engineEnv.anyOverride {
		cfg.Engine = &resolvedEngine
	}
	cfg.engineResolutionApplied = true
	cfg.engineProjectionEnabled = yamlEngineConfigured || engineEnv.hostOverride
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
	c.ResolveLargeRepoPresets()
	if err := validateInOrder(
		c.validateSchemaVersion,
		c.Workcopies.validate,
		func() error { return c.API.validate(c.APIListenAddress()) },
		c.validateWorkflowSource,
		func() error { return c.Webhook.validate(c.WebhookListenAddress()) },
	); err != nil {
		return err
	}

	// Store declarations must validate before any section checks a store-backed token.
	stores, err := c.validateSecretStores()
	if err != nil {
		return err
	}
	return validateInOrder(
		func() error { return c.Portal.validate() },
		c.validateSpeech,
		func() error { return c.Webhook.validateSecret(stores) },
		c.validateTimezone,
		c.Runner.validateDefaultStageTimeout,
		func() error { return c.Telemetry.validate(stores, c.TelemetryEnabled()) },
		c.validateExternalTelemetry,
		c.Telemetry.Retention.validate,
		c.RunConditions.validate,
		c.Retention.validate,
		func() error { return c.validateRepos(stores) },
		func() error { return c.validateDaemonIdentity(stores) },
		func() error { return c.validateCredentials(stores) },
		c.Runner.validate,
		c.validateRunners,
		func() error { return c.validateWorkflowSourceCredentials(stores) },
		c.validateSandbox,
	)
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
		if err := s.validate(i, stores); err != nil {
			return nil, err
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

// Validate checks portal co-branding configuration shape and URL safety.
func (p PortalConfig) Validate() error {
	if len(p.Brand.Name) > 64 {
		return fmt.Errorf("brand.name must be 64 characters or fewer")
	}
	if len(p.Brand.Tagline) > 128 {
		return fmt.Errorf("brand.tagline must be 128 characters or fewer")
	}
	if p.Brand.LogoURL != "" && !strings.HasPrefix(p.Brand.LogoURL, "/assets/") {
		return fmt.Errorf("brand.logoUrl must start with /assets/")
	}
	if p.Brand.FaviconURL != "" && !strings.HasPrefix(p.Brand.FaviconURL, "/assets/") {
		return fmt.Errorf("brand.faviconUrl must start with /assets/")
	}
	for name, value := range map[string]string{
		"theme.accentLight":     p.Theme.AccentLight,
		"theme.accentDark":      p.Theme.AccentDark,
		"theme.accentSoftLight": p.Theme.AccentSoftLight,
		"theme.accentSoftDark":  p.Theme.AccentSoftDark,
		"theme.accentInkLight":  p.Theme.AccentInkLight,
		"theme.accentInkDark":   p.Theme.AccentInkDark,
	} {
		if value != "" && !validPortalCSSColor(value) {
			return fmt.Errorf("%s must be a plausible CSS color", name)
		}
	}
	if p.Support.DocsURL != "" && !strings.HasPrefix(p.Support.DocsURL, "https://") {
		return fmt.Errorf("support.docsUrl must start with https://")
	}
	if p.Support.IssuesURL != "" && !strings.HasPrefix(p.Support.IssuesURL, "https://") {
		return fmt.Errorf("support.issuesUrl must start with https://")
	}
	if p.Support.ChatURL != "" &&
		!strings.HasPrefix(p.Support.ChatURL, "https://") &&
		!strings.HasPrefix(p.Support.ChatURL, "slack://") &&
		!strings.HasPrefix(p.Support.ChatURL, "msteams://") {
		return fmt.Errorf("support.chatUrl must start with https://, slack://, or msteams://")
	}
	if len(p.Support.Links) > 6 {
		return fmt.Errorf("support.links must contain 6 entries or fewer")
	}
	for i, link := range p.Support.Links {
		if strings.TrimSpace(link.Label) == "" {
			return fmt.Errorf("support.links[%d].label is required", i)
		}
		if len(link.Label) > 32 {
			return fmt.Errorf("support.links[%d].label must be 32 characters or fewer", i)
		}
		if !strings.HasPrefix(link.URL, "https://") {
			return fmt.Errorf("support.links[%d].url must start with https://", i)
		}
	}
	return nil
}

// Validate checks workflow-source shape without resolving credentials or
// accessing the source.
func (s WorkflowSource) Validate() error {
	return s.validate()
}

func (s WorkflowSource) validate() error {
	hasPath := s.Path != ""
	hasURL := s.URL != ""

	switch s.Kind {
	case WorkflowSourceKindLocalDir:
		if !hasPath {
			return fmt.Errorf("path is required for kind %q", s.Kind)
		}
		if hasURL || s.Ref != "" || s.Token != nil || s.Auth != nil {
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
			if s.Auth != nil {
				if err := s.validateAuth(); err != nil {
					return err
				}
			} else if s.Token == nil || s.Token.sourceCount() != 1 {
				return fmt.Errorf("remote git token must reference exactly one of env, file, keychain, or store — inline secret values are never permitted (CFG-009, SEC-010)")
			}
		} else {
			if s.Token != nil {
				return fmt.Errorf("token is only valid for a remote git url")
			}
			if s.Auth != nil {
				return fmt.Errorf("auth is only valid for a remote git url")
			}
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

// validateAuth checks a remote git workflowSource's auth block (#3274). The
// block reuses RepoAuthConfig, but only github-app is meaningful here: a
// static credential is spelled token:, never auth kind pat, so the two can
// never compete for the same fetch. Required-field and mutual-exclusion
// wording mirrors repos[]' own github-app validation; the stores/envPassthrough
// checks the Config-level pass owns live in validateWorkflowSourceCredentials.
func (s WorkflowSource) validateAuth() error {
	if s.Auth.Kind != GitHubAuthApp {
		return fmt.Errorf("unsupported auth kind %q (supported: %q; a static credential is configured through token, not auth)", s.Auth.Kind, GitHubAuthApp)
	}
	if s.Token != nil {
		return fmt.Errorf("auth kind %q must not configure token.env, token.file, token.keychain, or token.store — the installation token is minted", GitHubAuthApp)
	}
	if s.Auth.Tenant != "" || s.Auth.ClientID != "" {
		return fmt.Errorf("auth.tenant and auth.clientId are only valid for ADO auth kinds")
	}
	if s.Auth.AppID == "" {
		return fmt.Errorf("auth.appId is required for auth kind %q", GitHubAuthApp)
	}
	if s.Auth.InstallationID == "" {
		return fmt.Errorf("auth.installationId is required for auth kind %q", GitHubAuthApp)
	}
	if _, err := strconv.ParseUint(string(s.Auth.InstallationID), 10, 64); err != nil {
		return fmt.Errorf("auth.installationId %q must be the numeric installation ID", s.Auth.InstallationID)
	}
	if s.Auth.PrivateKey == nil || s.Auth.PrivateKey.sourceCount() != 1 {
		return fmt.Errorf("auth.privateKey must reference exactly one of env, file, keychain, or store — " +
			"inline secret values are never permitted (CFG-009, SEC-010)")
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

func validPortalCSSColor(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed != value {
		return false
	}
	if strings.ContainsAny(trimmed, ";{}") {
		return false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(lower, "rgb") || strings.HasPrefix(lower, "hsl") {
		return true
	}
	return knownPortalCSSNamedColors[lower]
}

var knownPortalCSSNamedColors = map[string]bool{
	"aliceblue":            true,
	"antiquewhite":         true,
	"aqua":                 true,
	"aquamarine":           true,
	"azure":                true,
	"beige":                true,
	"bisque":               true,
	"black":                true,
	"blanchedalmond":       true,
	"blue":                 true,
	"blueviolet":           true,
	"brown":                true,
	"burlywood":            true,
	"cadetblue":            true,
	"chartreuse":           true,
	"chocolate":            true,
	"coral":                true,
	"cornflowerblue":       true,
	"cornsilk":             true,
	"crimson":              true,
	"cyan":                 true,
	"darkblue":             true,
	"darkcyan":             true,
	"darkgoldenrod":        true,
	"darkgray":             true,
	"darkgreen":            true,
	"darkgrey":             true,
	"darkkhaki":            true,
	"darkmagenta":          true,
	"darkolivegreen":       true,
	"darkorange":           true,
	"darkorchid":           true,
	"darkred":              true,
	"darksalmon":           true,
	"darkseagreen":         true,
	"darkslateblue":        true,
	"darkslategray":        true,
	"darkslategrey":        true,
	"darkturquoise":        true,
	"darkviolet":           true,
	"deeppink":             true,
	"deepskyblue":          true,
	"dimgray":              true,
	"dimgrey":              true,
	"dodgerblue":           true,
	"firebrick":            true,
	"floralwhite":          true,
	"forestgreen":          true,
	"fuchsia":              true,
	"gainsboro":            true,
	"ghostwhite":           true,
	"gold":                 true,
	"goldenrod":            true,
	"gray":                 true,
	"green":                true,
	"greenyellow":          true,
	"grey":                 true,
	"honeydew":             true,
	"hotpink":              true,
	"indianred":            true,
	"indigo":               true,
	"ivory":                true,
	"khaki":                true,
	"lavender":             true,
	"lavenderblush":        true,
	"lawngreen":            true,
	"lemonchiffon":         true,
	"lightblue":            true,
	"lightcoral":           true,
	"lightcyan":            true,
	"lightgoldenrodyellow": true,
	"lightgray":            true,
	"lightgreen":           true,
	"lightgrey":            true,
	"lightpink":            true,
	"lightsalmon":          true,
	"lightseagreen":        true,
	"lightskyblue":         true,
	"lightslategray":       true,
	"lightslategrey":       true,
	"lightsteelblue":       true,
	"lightyellow":          true,
	"lime":                 true,
	"limegreen":            true,
	"linen":                true,
	"magenta":              true,
	"maroon":               true,
	"mediumaquamarine":     true,
	"mediumblue":           true,
	"mediumorchid":         true,
	"mediumpurple":         true,
	"mediumseagreen":       true,
	"mediumslateblue":      true,
	"mediumspringgreen":    true,
	"mediumturquoise":      true,
	"mediumvioletred":      true,
	"midnightblue":         true,
	"mintcream":            true,
	"mistyrose":            true,
	"moccasin":             true,
	"navajowhite":          true,
	"navy":                 true,
	"oldlace":              true,
	"olive":                true,
	"olivedrab":            true,
	"orange":               true,
	"orangered":            true,
	"orchid":               true,
	"palegoldenrod":        true,
	"palegreen":            true,
	"paleturquoise":        true,
	"palevioletred":        true,
	"papayawhip":           true,
	"peachpuff":            true,
	"peru":                 true,
	"pink":                 true,
	"plum":                 true,
	"powderblue":           true,
	"purple":               true,
	"rebeccapurple":        true,
	"red":                  true,
	"rosybrown":            true,
	"royalblue":            true,
	"saddlebrown":          true,
	"salmon":               true,
	"sandybrown":           true,
	"seagreen":             true,
	"seashell":             true,
	"sienna":               true,
	"silver":               true,
	"skyblue":              true,
	"slateblue":            true,
	"slategray":            true,
	"slategrey":            true,
	"snow":                 true,
	"springgreen":          true,
	"steelblue":            true,
	"tan":                  true,
	"teal":                 true,
	"thistle":              true,
	"tomato":               true,
	"transparent":          true,
	"turquoise":            true,
	"violet":               true,
	"wheat":                true,
	"white":                true,
	"whitesmoke":           true,
	"yellow":               true,
	"yellowgreen":          true,
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
		return fmt.Errorf("insecure mode is allowed only for localhost or a loopback IP " +
			"(run a loopback sidecar collector, or point endpoint at a TLS collector and drop insecure: true)")
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

// Validate checks the OIDC issuer/audience/role-mapping shape without
// contacting the issuer.
func (c OIDCAuthConfig) Validate() error {
	if c.Issuer == "" {
		return fmt.Errorf("issuer is required")
	}
	issuer, err := url.Parse(c.Issuer)
	if err != nil || !issuer.IsAbs() || issuer.Host == "" {
		return fmt.Errorf("issuer must be an absolute http(s) URL")
	}
	switch strings.ToLower(issuer.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(issuer.Hostname()) {
			return fmt.Errorf("issuer %q must use https; http is allowed only for a loopback development issuer", c.Issuer)
		}
	default:
		return fmt.Errorf("issuer must be an absolute http(s) URL")
	}
	if issuer.RawQuery != "" || issuer.Fragment != "" {
		return fmt.Errorf("issuer must not carry a query or fragment")
	}
	if c.Audience == "" {
		return fmt.Errorf("audience is required")
	}
	mapped := 0
	seen := make(map[string]string)
	for _, group := range []struct {
		role   string
		values []string
	}{
		{role: "view", values: c.Roles.View},
		{role: "operate", values: c.Roles.Operate},
		{role: "admin", values: c.Roles.Admin},
	} {
		for _, value := range group.values {
			if value == "" {
				return fmt.Errorf("roles.%s must not contain empty claim values", group.role)
			}
			if prior, duplicate := seen[value]; duplicate {
				return fmt.Errorf("claim value %q is mapped to both %s and %s; roles are ordered (admin ⊇ operate ⊇ view), map each value once", value, prior, group.role)
			}
			seen[value] = group.role
			mapped++
		}
	}
	if mapped == 0 {
		return fmt.Errorf("roles must map at least one claim value to view, operate, or admin — an empty mapping denies every principal")
	}
	return nil
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

// IsLoopbackListenAddress reports whether address is a valid loopback
// host:port listener.
func IsLoopbackListenAddress(address string) bool {
	return validateLoopbackListenAddress(address) == nil
}

// WriteConfig marshals cfg as YAML and writes it to path.
func WriteConfig(path string, cfg *Config) error {
	yamlBytes, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func marshalConfig(cfg *Config) ([]byte, error) {
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal instance config: %w", err)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("marshal instance config: %w", err)
	}
	return yamlBytes, nil
}
