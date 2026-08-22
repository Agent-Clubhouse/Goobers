package instance

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runnercap"
)

func validateInOrder(validators ...func() error) error {
	for _, validate := range validators {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c *WorkcopiesConfig) validate() error {
	if c != nil && c.Root != "" && !filepath.IsAbs(c.Root) {
		return fmt.Errorf("workcopies.root must be an absolute path: %q", c.Root)
	}
	return nil
}

func (c APIConfig) validate(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("api.listen: must be a host:port address: %w", err)
	}
	if host == "" {
		return fmt.Errorf("api.listen: host is required; wildcard listeners are not allowed")
	}
	if number, err := strconv.Atoi(port); err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("api.listen: port %q must be a number from 0 through 65535", port)
	}
	if c.TLS != nil && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return fmt.Errorf("api.tls: certFile and keyFile are both required")
	}
	if c.Auth != nil {
		if c.Auth.OIDC == nil {
			return fmt.Errorf("api.auth: oidc is required — it is the only supported authenticator (SEC-043)")
		}
		if err := c.Auth.OIDC.Validate(); err != nil {
			return fmt.Errorf("api.auth.oidc: %w", err)
		}
	}
	if !isLoopbackHost(host) && (c.TLS == nil || c.Auth == nil) {
		return fmt.Errorf("api.listen: host %q is not loopback: exposing the daemon API off-loopback requires "+
			"both api.tls (certFile + keyFile) and api.auth.oidc so the listener is encrypted and authenticated; "+
			"there is no insecure override — bind a loopback address instead (SEC-043, #640)", host)
	}
	return nil
}

func (c *Config) validateWorkflowSource() error {
	if c.WorkflowSource == nil {
		return nil
	}
	if err := c.WorkflowSource.Validate(); err != nil {
		return fmt.Errorf("workflowSource: %w", err)
	}
	return nil
}

func (c WebhookConfig) validate(address string) error {
	if err := validateLoopbackListenAddress(address); err != nil {
		return fmt.Errorf("webhook.listen: %w", err)
	}
	return nil
}

func (c WebhookConfig) validateSecret(stores map[string]bool) error {
	if c.Secret.sourceCount() > 1 {
		return fmt.Errorf("webhook.secret must reference exactly one of env, file, keychain, or store — inline secret values are never permitted (CFG-009, SEC-010)")
	}
	return validateStoreRef("webhook.secret", c.Secret, stores)
}

func (c SecretStoreConfig) validate(i int, stores map[string]bool) error {
	if c.Name == "" {
		return fmt.Errorf("secretStores[%d]: name is required", i)
	}
	if !validSecretStoreName(c.Name) {
		return fmt.Errorf("secretStores[%d]: name %q must be a lowercase DNS label (letters, digits, and interior hyphens, at most 63 characters)", i, c.Name)
	}
	if stores[c.Name] {
		return fmt.Errorf("secretStores[%d]: name %q is declared more than once", i, c.Name)
	}
	stores[c.Name] = true
	if c.Kind != SecretStoreKindAzureKeyVault {
		return fmt.Errorf("secretStores[%d] (%s): unsupported kind %q (supported: %q)", i, c.Name, c.Kind, SecretStoreKindAzureKeyVault)
	}
	if err := validateVaultURI(c.VaultURI); err != nil {
		return fmt.Errorf("secretStores[%d] (%s): vaultURI: %w", i, c.Name, err)
	}
	if c.Auth == nil {
		return fmt.Errorf("secretStores[%d] (%s): auth is required (kind: one of %q, %q, %q) — store access always authenticates through an ambient identity, never a token ref",
			i, c.Name, SecretStoreAuthWorkloadIdentity, SecretStoreAuthManagedIdentity, SecretStoreAuthAzureCLI)
	}
	switch c.Auth.Kind {
	case SecretStoreAuthWorkloadIdentity, SecretStoreAuthManagedIdentity:
	case SecretStoreAuthAzureCLI:
		if c.Auth.ClientID != "" {
			return fmt.Errorf("secretStores[%d] (%s): auth.clientId is not valid for auth kind %q", i, c.Name, c.Auth.Kind)
		}
	default:
		return fmt.Errorf("secretStores[%d] (%s): unsupported auth kind %q (supported: %q, %q, %q)",
			i, c.Name, c.Auth.Kind, SecretStoreAuthWorkloadIdentity, SecretStoreAuthManagedIdentity, SecretStoreAuthAzureCLI)
	}
	if c.CacheTTLSeconds < 0 {
		return fmt.Errorf("secretStores[%d] (%s): cacheTTLSeconds must not be negative", i, c.Name)
	}
	return nil
}

func (p PortalConfig) validate() error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("portal: %w", err)
	}
	return nil
}

func (c *Config) validateSpeech() error {
	if c.Speech == nil {
		return nil
	}
	if err := c.Speech.Validate(); err != nil {
		return fmt.Errorf("speech: %w", err)
	}
	return nil
}

func (c *Config) validateTimezone() error {
	if c.Timezone == "" {
		return nil
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	return nil
}

func (c RunnerConfig) validateDefaultStageTimeout() error {
	_, err := c.DefaultStageTimeoutDuration()
	return err
}

func (c TelemetryConfig) validate(stores map[string]bool, telemetryEnabled bool) error {
	if c.OTLP == nil {
		return nil
	}
	if err := c.OTLP.Validate(); err != nil {
		return fmt.Errorf("telemetry.otlp: %w", err)
	}
	if c.OTLP.Enabled() && !telemetryEnabled {
		return fmt.Errorf("telemetry.otlp.endpoint cannot be set when telemetry.enabled is false")
	}
	names := make([]string, 0, len(c.OTLP.Headers))
	for name := range c.OTLP.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateStoreRef(fmt.Sprintf("telemetry.otlp.headers[%q]", name), c.OTLP.Headers[name], stores); err != nil {
			return err
		}
	}
	return nil
}

func (c *TelemetryRetentionConfig) validate() error {
	if c == nil {
		return nil
	}
	if _, err := c.WindowDuration(); err != nil {
		return err
	}
	if c.MaxRuns < 0 {
		return fmt.Errorf("telemetry.retention.maxRuns must not be negative")
	}
	return nil
}

func (c *Config) validateExternalTelemetry() error {
	if err := c.ExternalTelemetry.Validate(); err != nil {
		return fmt.Errorf("externalTelemetry: %w", err)
	}
	for i, connector := range c.ExternalTelemetry.Connectors {
		if connector.Auth.Token != nil &&
			connector.Auth.Token.Env != "" &&
			stageEnvironmentAllows(connector.Auth.Token.Env, c.Runner.EnvPassthrough) {
			return fmt.Errorf(
				"externalTelemetry.connectors[%d] (%s): auth.token.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
				i, connector.Name, connector.Auth.Token.Env,
			)
		}
	}
	return nil
}

func (c RunConditions) validate() error {
	if _, err := c.StalledRunTimeoutDuration(); err != nil {
		return err
	}
	if err := runcontrol.Validate("runConditions", c.RunControls()); err != nil {
		return err
	}
	_, err := c.ClaimsLockTimeoutDuration()
	return err
}

func (c RetentionConfig) validate() error {
	if c.MaxRetainedWorktreeBytes < 0 {
		return fmt.Errorf("retention.maxRetainedWorktreeBytes must not be negative")
	}
	if _, err := c.RetainedWorktreeMaxAgeDuration(); err != nil {
		return err
	}
	if c.Enabled && c.MaxRetainedWorktreeBytes == 0 && c.RetainedWorktreeMaxAge == "" {
		return fmt.Errorf("retention.enabled requires at least one of retention.maxRetainedWorktreeBytes or retention.retainedWorktreeMaxAge to be set (enabling retention with no limits prunes nothing)")
	}
	return nil
}

func (c *Config) validateRepos(stores map[string]bool) error {
	for i := range c.Repos {
		if err := c.Repos[i].validate(i, stores, c.Runner.EnvPassthrough); err != nil {
			return err
		}
	}
	return nil
}

func (r RepoRef) validate(i int, stores map[string]bool, envPassthrough []string) error {
	return validateInOrder(
		func() error { return r.validateIdentity(i) },
		func() error { return r.PathLength.validate(i, r) },
		func() error { return r.validateTimeout(i) },
		func() error { return r.validateRunControls(i) },
		func() error { return r.Workspace.validate(i, r) },
		func() error { return r.validateToken(i, stores) },
		func() error { return r.validateProvider(i, stores, envPassthrough) },
		func() error { return r.Policy.validate(i, r) },
	)
}

func (r RepoRef) validateIdentity(i int) error {
	switch r.Provider {
	case "github", "ado", "gitea":
	default:
		return fmt.Errorf("repos[%d]: unsupported provider %q (supported: \"github\", \"ado\", \"gitea\")", i, r.Provider)
	}
	if r.Owner == "" || r.Name == "" {
		return fmt.Errorf("repos[%d]: owner and name are required", i)
	}
	return nil
}

func (c *RepoPathLengthConfig) validate(i int, r RepoRef) error {
	if c == nil {
		return nil
	}
	if c.MaxPathLength < 0 {
		return fmt.Errorf("repos[%d] (%s/%s): pathLength.maxPathLength must not be negative", i, r.Owner, r.Name)
	}
	if c.BuildOutputAllowance < 0 {
		return fmt.Errorf("repos[%d] (%s/%s): pathLength.buildOutputAllowance must not be negative", i, r.Owner, r.Name)
	}
	return nil
}

func (r RepoRef) validateTimeout(i int) error {
	if r.DefaultStageTimeout == "" {
		return nil
	}
	if _, err := (RunnerConfig{DefaultStageTimeout: r.DefaultStageTimeout}).DefaultStageTimeoutDuration(); err != nil {
		return fmt.Errorf("repos[%d] (%s/%s): %w", i, r.Owner, r.Name, err)
	}
	return nil
}

func (r RepoRef) validateRunControls(i int) error {
	if r.RunControls == nil {
		return nil
	}
	return runcontrol.Validate(fmt.Sprintf("repos[%d].runControls", i), *r.RunControls)
}

func (c *RepoWorkspaceConfig) validate(i int, r RepoRef) error {
	if c == nil {
		return nil
	}
	if c.Pinned && c.Worktrees {
		return fmt.Errorf("VER: repos[%d] (%s/%s): workspace.pinned and workspace.worktrees are mutually exclusive", i, r.Owner, r.Name)
	}
	switch c.CleanPolicy {
	case "", WorkspaceCleanNone, WorkspaceCleanIgnoredSafe, WorkspaceCleanFull:
	default:
		return fmt.Errorf("VER: repos[%d] (%s/%s): workspace.cleanPolicy %q must be one of none, ignored-safe, or full", i, r.Owner, r.Name, c.CleanPolicy)
	}
	if !r.Pinned() && c.CleanPolicy != "" {
		return fmt.Errorf("VER: repos[%d] (%s/%s): workspace.cleanPolicy requires workspace.pinned", i, r.Owner, r.Name)
	}
	return nil
}

func (r RepoRef) validateToken(i int, stores map[string]bool) error {
	if r.Token.sourceCount() > 1 {
		return fmt.Errorf("repos[%d] (%s/%s): token must reference exactly one of env, file, keychain, or store — "+
			"inline secret values are never permitted (CFG-009, SEC-010)", i, r.Owner, r.Name)
	}
	return validateStoreRef(fmt.Sprintf("repos[%d] (%s/%s): token", i, r.Owner, r.Name), r.Token, stores)
}

func (r RepoRef) validateProvider(i int, stores map[string]bool, envPassthrough []string) error {
	switch r.Provider {
	case "github":
		return r.validateGitHub(i, stores, envPassthrough)
	case "ado":
		return r.validateADO(i)
	case "gitea":
		return r.validateGitea(i)
	default:
		return nil
	}
}

func (r RepoRef) validateGitHub(i int, stores map[string]bool, envPassthrough []string) error {
	if r.Project != "" {
		return fmt.Errorf("repos[%d] (%s/%s): project is only valid for provider \"ado\"", i, r.Owner, r.Name)
	}
	kind := GitHubAuthPAT
	if r.Auth != nil {
		kind = r.Auth.Kind
	}
	switch kind {
	case GitHubAuthPAT:
		if r.Auth != nil && r.Auth.hasGitHubAppFields() {
			return fmt.Errorf("repos[%d] (%s/%s): auth.appId, auth.installationId, and auth.privateKey are only valid for auth kind %q", i, r.Owner, r.Name, GitHubAuthApp)
		}
		if !r.Token.Configured() {
			return fmt.Errorf("repos[%d] (%s/%s): token must reference exactly one of env, file, keychain, or store — "+
				"inline secret values are never permitted (CFG-009, SEC-010)", i, r.Owner, r.Name)
		}
		return nil
	case GitHubAuthApp:
		return r.validateGitHubApp(i, stores, envPassthrough)
	default:
		return fmt.Errorf("repos[%d] (%s/%s): unsupported GitHub auth kind %q (supported: %q, %q)", i, r.Owner, r.Name, kind, GitHubAuthPAT, GitHubAuthApp)
	}
}

func (r RepoRef) validateGitHubApp(i int, stores map[string]bool, envPassthrough []string) error {
	if r.Token.Configured() {
		return fmt.Errorf("repos[%d] (%s/%s): auth kind %q must not configure token.env, token.file, token.keychain, or token.store — the installation token is minted", i, r.Owner, r.Name, GitHubAuthApp)
	}
	if r.Auth.Tenant != "" || r.Auth.ClientID != "" {
		return fmt.Errorf("repos[%d] (%s/%s): auth.tenant and auth.clientId are only valid for ADO auth kinds", i, r.Owner, r.Name)
	}
	if r.Auth.AppID == "" {
		return fmt.Errorf("repos[%d] (%s/%s): auth.appId is required for auth kind %q", i, r.Owner, r.Name, GitHubAuthApp)
	}
	if r.Auth.InstallationID == "" {
		return fmt.Errorf("repos[%d] (%s/%s): auth.installationId is required for auth kind %q", i, r.Owner, r.Name, GitHubAuthApp)
	}
	if _, err := strconv.ParseUint(string(r.Auth.InstallationID), 10, 64); err != nil {
		return fmt.Errorf("repos[%d] (%s/%s): auth.installationId %q must be the numeric installation ID", i, r.Owner, r.Name, r.Auth.InstallationID)
	}
	if r.Auth.PrivateKey == nil || r.Auth.PrivateKey.sourceCount() != 1 {
		return fmt.Errorf("repos[%d] (%s/%s): auth.privateKey must reference exactly one of env, file, keychain, or store — "+
			"inline secret values are never permitted (CFG-009, SEC-010)", i, r.Owner, r.Name)
	}
	if err := validateStoreRef(fmt.Sprintf("repos[%d] (%s/%s): auth.privateKey", i, r.Owner, r.Name), *r.Auth.PrivateKey, stores); err != nil {
		return err
	}
	if r.Auth.PrivateKey.Env != "" && stageEnvironmentAllows(r.Auth.PrivateKey.Env, envPassthrough) {
		return fmt.Errorf(
			"repos[%d] (%s/%s): auth.privateKey.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
			i, r.Owner, r.Name, r.Auth.PrivateKey.Env,
		)
	}
	return nil
}

func (r RepoRef) validateADO(i int) error {
	if r.Project == "" {
		return fmt.Errorf("repos[%d] (%s/%s): project is required for provider \"ado\"", i, r.Owner, r.Name)
	}
	if r.Auth != nil && r.Auth.hasGitHubAppFields() {
		return fmt.Errorf("repos[%d] (%s/%s): auth.appId, auth.installationId, and auth.privateKey are only valid for provider \"github\"", i, r.Owner, r.Name)
	}
	kind := ADOAuthPAT
	if r.Auth != nil {
		kind = r.Auth.Kind
	}
	switch kind {
	case ADOAuthPAT:
		if !r.Token.Configured() {
			return fmt.Errorf("repos[%d] (%s/%s): ADO PAT auth requires token.env, token.file, token.keychain, or token.store", i, r.Owner, r.Name)
		}
	case ADOAuthAzureCLI, ADOAuthWorkloadIdentity, ADOAuthManagedIdentity:
		if r.Token.Configured() {
			return fmt.Errorf("repos[%d] (%s/%s): ADO auth kind %q must not configure token.env, token.file, token.keychain, or token.store", i, r.Owner, r.Name, kind)
		}
	default:
		return fmt.Errorf("repos[%d] (%s/%s): unsupported ADO auth kind %q", i, r.Owner, r.Name, kind)
	}
	if r.Auth != nil && r.Auth.ClientID != "" && kind != ADOAuthManagedIdentity {
		return fmt.Errorf("repos[%d] (%s/%s): auth.clientId is only valid for managed-identity", i, r.Owner, r.Name)
	}
	return nil
}

func (r RepoRef) validateGitea(i int) error {
	if r.BaseURL == "" {
		return fmt.Errorf("repos[%d] (%s/%s): baseUrl is required for provider \"gitea\" (self-hosted Gitea has no fixed host)", i, r.Owner, r.Name)
	}
	if r.Project != "" {
		return fmt.Errorf("repos[%d] (%s/%s): project is only valid for provider \"ado\"", i, r.Owner, r.Name)
	}
	if r.Auth != nil {
		return fmt.Errorf("repos[%d] (%s/%s): provider \"gitea\" supports only a static token; remove the auth block", i, r.Owner, r.Name)
	}
	if !r.Token.Configured() {
		return fmt.Errorf("repos[%d] (%s/%s): gitea auth requires token.env, token.file, token.keychain, or token.store", i, r.Owner, r.Name)
	}
	return nil
}

func (p *RepoPolicyExpectation) validate(i int, r RepoRef) error {
	if p == nil {
		return nil
	}
	if r.Provider != "github" {
		return fmt.Errorf("repos[%d] (%s/%s): policy is only supported for provider \"github\" (issue #916 V1 scope)", i, r.Owner, r.Name)
	}
	switch p.RequiredMergeMethod {
	case "", "merge", "squash", "rebase":
	default:
		return fmt.Errorf("repos[%d] (%s/%s): policy.requiredMergeMethod must be \"\", \"merge\", \"squash\", or \"rebase\"", i, r.Owner, r.Name)
	}
	for _, check := range p.RequiredStatusChecks {
		if strings.TrimSpace(check) == "" {
			return fmt.Errorf("repos[%d] (%s/%s): policy.requiredStatusChecks entries must not be empty", i, r.Owner, r.Name)
		}
	}
	return nil
}

func (c *Config) validateDaemonIdentity(stores map[string]bool) error {
	if c.DaemonIdentity == nil {
		return nil
	}
	if err := c.DaemonIdentity.validate(c.Runner.EnvPassthrough, stores); err != nil {
		return fmt.Errorf("daemonIdentity: %w", err)
	}
	if err := c.validateDaemonIdentityOwnerCoverage(); err != nil {
		return err
	}
	return c.validateDaemonIdentitySameAppInstallations()
}

// validateDaemonIdentityOwnerCoverage rejects a single-installation GitHub App
// daemon identity on an instance whose GitHub repos span more than one owner
// (#3414, F1 of the multi-owner design).
//
// A GitHub App installation belongs to exactly one owner, so a single
// installationId can mint for at most one of them. Every such config is already
// runtime-fatal — it fails with a 422 at the first cross-owner mint (#3341) —
// which means rejecting it at load can only hit configs that never worked. That
// is why this is an error rather than a warning: it is strictly-better
// enforcement, and it converts a confusing mid-run failure into a startup
// message.
//
// The uncovered owners cannot be named individually, because an installationId
// is opaque and config alone cannot say which owner it belongs to. So the
// message names every owner in play and states the arithmetic.
func (c *Config) validateDaemonIdentityOwnerCoverage() error {
	if !c.DaemonIdentity.GitHubApp() || c.DaemonIdentity.InstallationID == "" {
		return nil
	}
	seen := make(map[string]bool)
	var owners []string
	for i := range c.Repos {
		repo := &c.Repos[i]
		if repo.Provider != "github" || repo.Owner == "" {
			continue
		}
		if !seen[repo.Owner] {
			seen[repo.Owner] = true
			owners = append(owners, repo.Owner)
		}
	}
	if len(owners) < 2 {
		return nil
	}
	sort.Strings(owners)
	return fmt.Errorf(
		"daemonIdentity: kind %q with a single installationId cannot cover repos across %d owners (%s) — "+
			"a GitHub App installation belongs to exactly one owner, so at most one of these can mint and "+
			"the rest fail at first use; configure a per-owner installation binding or split the instance",
		GitHubAuthApp, len(owners), strings.Join(owners, ", "))
}

// validateDaemonIdentitySameAppInstallations rejects a repo that authenticates
// as the SAME GitHub App as the daemon identity but declares a different
// installationId (#3414).
//
// GitHub allows one installation per (App, owner) pair, so if both halves of
// the config name the same App and disagree on the installation, one of them is
// wrong. This deliberately does not presume which: the message reports the
// disagreement and leaves the choice to the operator, because either half could
// be the stale one.
func (c *Config) validateDaemonIdentitySameAppInstallations() error {
	if !c.DaemonIdentity.GitHubApp() || c.DaemonIdentity.AppID == "" || c.DaemonIdentity.InstallationID == "" {
		return nil
	}
	for i := range c.Repos {
		repo := &c.Repos[i]
		if repo.Provider != "github" || repo.Auth == nil || repo.Auth.Kind != GitHubAuthApp {
			continue
		}
		if repo.Auth.AppID != c.DaemonIdentity.AppID {
			continue
		}
		if repo.Auth.InstallationID == "" || repo.Auth.InstallationID == c.DaemonIdentity.InstallationID {
			continue
		}
		return fmt.Errorf(
			"repos[%d] (%s/%s): auth.installationId %q disagrees with daemonIdentity.installationId %q for the same appId %q — "+
				"GitHub allows one installation per App per owner, so one of these is wrong",
			i, repo.Owner, repo.Name, repo.Auth.InstallationID, c.DaemonIdentity.InstallationID, c.DaemonIdentity.AppID)
	}
	return nil
}

func (c *Config) validateCredentials(stores map[string]bool) error {
	seen := make(map[string]bool, len(c.Credentials))
	for i := range c.Credentials {
		if err := c.Credentials[i].validate(i, seen, stores, c.Runner.EnvPassthrough); err != nil {
			return err
		}
	}
	return nil
}

func (c CredentialGrant) validate(i int, seen map[string]bool, stores map[string]bool, envPassthrough []string) error {
	key, label, err := c.identity(i)
	if err != nil {
		return err
	}
	if seen[key] {
		return fmt.Errorf("credentials[%d]: %s is sourced more than once", i, label)
	}
	seen[key] = true
	if c.Token.sourceCount() != 1 {
		return fmt.Errorf("credentials[%d] (%s): token must reference exactly one of env, file, keychain, or store — "+
			"inline secret values are never permitted (CFG-009, SEC-010)", i, label)
	}
	if c.Token.Env != "" && stageEnvironmentAllows(c.Token.Env, envPassthrough) {
		return fmt.Errorf(
			"credentials[%d] (%s): token.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
			i, label, c.Token.Env,
		)
	}
	return validateStoreRef(fmt.Sprintf("credentials[%d] (%s): token", i, label), c.Token, stores)
}

func (c CredentialGrant) identity(i int) (string, string, error) {
	switch {
	case c.Capability != "" && c.MCP == "":
		if !capability.Known(c.Capability) {
			return "", "", fmt.Errorf("credentials[%d]: unknown capability %q", i, c.Capability)
		}
		if !capability.StageDeclarable(c.Capability) {
			return "", "", fmt.Errorf("credentials[%d]: capability %q is runner-owned; configure it through workflowSource.token", i, c.Capability)
		}
		return c.Capability, "capability " + strconv.Quote(c.Capability), nil
	case c.Capability == "" && mcpconfig.ValidBYOCredentialName(c.MCP):
		return mcpconfig.BYOCredentialKey(c.MCP), "MCP credential " + strconv.Quote(c.MCP), nil
	case c.Capability == "" && c.MCP != "":
		return "", "", fmt.Errorf("credentials[%d]: MCP credential name %q must be a lowercase DNS label", i, c.MCP)
	default:
		return "", "", fmt.Errorf("credentials[%d]: set exactly one of capability or mcp", i)
	}
}

func (c RunnerConfig) validate() error {
	for i, token := range c.Capabilities {
		if err := runnercap.ValidateToken(token); err != nil {
			return fmt.Errorf("runner.capabilities[%d]: %w", i, err)
		}
	}
	if _, err := c.LivenessTimeoutDuration(); err != nil {
		return err
	}
	for i, name := range c.EnvPassthrough {
		if !procenv.ValidName(name) {
			return fmt.Errorf("runner.envPassthrough[%d]: %q is not a valid environment variable name", i, name)
		}
	}
	for name, command := range c.HarnessCommand {
		if !knownHarnessName(name) {
			return fmt.Errorf("runner.harnessCommand[%q]: unknown harness (known: %s)", name, strings.Join(knownHarnessNames(), ", "))
		}
		if len(command) == 0 {
			return fmt.Errorf("runner.harnessCommand[%q]: command must not be empty", name)
		}
		if strings.TrimSpace(command[0]) == "" {
			return fmt.Errorf("runner.harnessCommand[%q]: program name (first element) must not be empty", name)
		}
	}
	return nil
}

// validateWorkflowSourceCredentials checks the credential refs a
// workflowSource carries — the static token or, for auth kind github-app
// (#3274), the App private key — against the declared secret stores and the
// stage environment. Structural auth checks (required fields, token/auth
// mutual exclusion) live in WorkflowSource.Validate; these need the stores set
// and runner.envPassthrough, which only this Config-level pass has.
func (c *Config) validateWorkflowSourceCredentials(stores map[string]bool) error {
	if c.WorkflowSource == nil {
		return nil
	}
	if token := c.WorkflowSource.Token; token != nil {
		if err := validateStoreRef("workflowSource.token", *token, stores); err != nil {
			return err
		}
		if token.Env != "" && stageEnvironmentAllows(token.Env, c.Runner.EnvPassthrough) {
			return fmt.Errorf(
				"workflowSource.token.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
				token.Env,
			)
		}
	}
	if auth := c.WorkflowSource.Auth; auth != nil && auth.PrivateKey != nil {
		if err := validateStoreRef("workflowSource.auth.privateKey", *auth.PrivateKey, stores); err != nil {
			return err
		}
		// The App key can mint tokens broadly — never allow the stage
		// environment to carry it, mirroring repos[] and daemonIdentity.
		if auth.PrivateKey.Env != "" && stageEnvironmentAllows(auth.PrivateKey.Env, c.Runner.EnvPassthrough) {
			return fmt.Errorf(
				"workflowSource.auth.privateKey.env %q must not be exposed to stages through runner.envPassthrough or the built-in process environment allowlist",
				auth.PrivateKey.Env,
			)
		}
	}
	return nil
}

func (c *Config) validateSandbox() error {
	if c.Sandbox == nil {
		return nil
	}
	if err := c.Sandbox.Validate(); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return nil
}
