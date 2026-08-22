package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/externaltelemetry/adx"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
	connectorapi "github.com/goobers/goobers/telemetryconnector/v1alpha1"
)

// repoCloneURL overrides runner.Config.RepoCloneURL when non-nil. It exists
// purely as a test seam (mirrors internal/localscheduler's swappable newRunID)
// so integration tests can point worktree provisioning at a local git fixture
// instead of a real GitHub clone; production leaves it nil and runner.New
// falls back to its own github.com default.
var repoCloneURL func(apiv1.RepoRef) (string, error)

// newAgenticAdapter overrides the adapter selected from the harness Registry
// for an agentic stage when non-nil. It is a test seam (mirroring
// repoCloneURL above) so the CLI-level acceptance check (acceptance_test.go)
// can substitute a fake for the real Copilot CLI subprocess and drive the full
// agentic loop — implement -> reviewer gate -> local-ci — through `goobers
// run`/`up` offline, extending #29's runner-API-level walking skeleton to the
// CLI entrypoint. Production leaves it nil.
var newAgenticAdapter func(gooberName string, envCaps map[string]string) harness.Adapter

// newPRPoller overrides how buildRunnerConfig constructs the ci-poll stage's
// PRPoller when non-nil. Test seam mirroring repoCloneURL/newAgenticAdapter
// above, so a CLI-level test can point ci-poll at a fake PR provider (an
// httptest.Server, or a bespoke fake) instead of a real GitHub token/network
// (#132). Production leaves it nil and buildRunnerConfig constructs a real
// providers.GitHubProvider over the resolved repo token.
var newPRPoller func(token string) executor.PRPoller

// credentialGrantEnv is the environment variable the Copilot CLI reads most
// credentialed capabilities' tokens from (internal/harness.CopilotAdapter's
// EnvCapabilities convention — matches internal/harness/copilot_test.go's
// {"repo:push": "GH_TOKEN"} fixture).
const credentialGrantEnv = "GH_TOKEN"

// copilotModelEnv is the environment variable the Copilot CLI reads its
// model-backend token from. The CLI prefers COPILOT_GITHUB_TOKEN over GH_TOKEN
// for model auth (§3.3), so mapping agent:model to a DISTINCT env var from
// credentialGrantEnv lets one agentic subprocess carry a personal "Copilot
// Requests" PAT for the model (agent:model → COPILOT_GITHUB_TOKEN) AND the
// org-repo token for the github tool (ordinary repo capabilities → GH_TOKEN)
// at once — credentialEnv appends both, and because the vars differ neither
// clobbers the other (#288, multi-token credentials 2/3).
const copilotModelEnv = "COPILOT_GITHUB_TOKEN"

const claudeModelEnv = "ANTHROPIC_API_KEY"

// credentialedCapabilities are the canonical capabilities (internal/capability,
// issue #74) a repo's token can satisfy; telemetry:read needs no credential.
var credentialedCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesRead, capability.GitHubIssuesWrite, capability.GitHubMilestonesWrite, capability.GitHubIssuesApprove, capability.ProviderPRWrite, capability.GitHubPRWrite, capability.GitHubPRReview, capability.GitHubBranchDelete, capability.GitHubPRMerge,
	// ADO PR completion authority is repo-token-backed like the GitHub grants
	// above; only stages that declare ado:pr:complete receive its credential,
	// preserving the decider/executor isolation (merge-review completes; the
	// implementation and remediation lanes never can).
	capability.ADOPRComplete,
}

// daemonIdentityRefName is the resolver ref name a configured DaemonIdentity's
// credential (static token or App-minted) is registered under (#1780),
// namespaced away from repo refs ("owner/name") and explicit credentials:
// refs ("credential:<key>") the same way those two are namespaced from each
// other.
const daemonIdentityRefName = "daemon-identity"

// daemonIdentityCapabilities are the standard daemon-mutation capabilities a
// configured DaemonIdentity backs by default (#1780) — deliberately a subset
// of credentialedCapabilities: GitHubMilestonesWrite (roadmap mutation) and
// GitHubIssuesApprove (the goobers:approved trust decision) are excluded
// because both are explicitly documented (internal/capability) as separate,
// deliberate decisions that must not silently follow whichever identity
// authors ordinary PRs/issues — an instance that wants those on the daemon
// identity too can still say so explicitly via credentials:.
var daemonIdentityCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesWrite, capability.GitHubPRWrite, capability.GitHubPRReview, capability.GitHubBranchDelete, capability.GitHubPRMerge,
}

// buildEnvCapabilities maps each capability the Copilot adapter injects to the
// environment variable that consumes its token. General org-repo capabilities
// use GH_TOKEN (the github tool's var), command-scoped capabilities use their
// dedicated GOOBERS_CRED_* variables, and agent:model uses
// COPILOT_GITHUB_TOKEN (the model backend's var, #288, §3.3).
func buildEnvCapabilities() map[string]string {
	envCaps := make(map[string]string, len(credentialedCapabilities)+1)
	for _, c := range credentialedCapabilities {
		envCaps[string(c)] = credentialGrantEnv
	}
	envCaps[string(capability.GitHubIssuesApprove)] = executor.CredentialEnvVar(string(capability.GitHubIssuesApprove))
	envCaps[string(capability.GitHubMilestonesWrite)] = executor.CredentialEnvVar(string(capability.GitHubMilestonesWrite))
	envCaps[string(capability.AgentModel)] = copilotModelEnv
	return envCaps
}

var copilotModelLister harness.CopilotModelLister

// buildHarnessRegistry is the production harness composition point. Registry
// keys are goober spec.harness values; adapter names remain their diagnostic
// identities, so Copilot continues to report "copilot-cli" in spans and errors.
func buildHarnessRegistry(envCaps map[string]string, envPassthrough []string, harnessCommand map[string][]string, instanceRoot, selfBin string, deferModelDiscovery bool) (*harness.Registry, error) {
	registry := harness.NewRegistry()
	copilotAdapter := &harness.CopilotAdapter{
		Command:         harnessCommandOrDefault(harnessCommand, string(apiv1.HarnessCopilot), []string{"copilot"}),
		AuthCheckArgs:   copilotAuthCheckArgs,
		ModelLister:     copilotModelLister,
		EnvCapabilities: envCaps,
		OptionalCredentialCapabilities: map[string]bool{
			string(capability.AgentModel): true,
		},
		ExtraEnvAllowlist: envPassthrough,
		InstanceRoot:      instanceRoot,
		SelfBin:           selfBin,
		DeferDiscovery:    deferModelDiscovery,
	}
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), copilotAdapter); err != nil {
		return nil, fmt.Errorf("register Copilot harness: %w", err)
	}

	claudeEnvCaps := make(map[string]string, len(envCaps)+1)
	for capability, envVar := range envCaps {
		claudeEnvCaps[capability] = envVar
	}
	claudeEnvCaps[string(capability.AgentModel)] = claudeModelEnv
	claudeAdapter := &harness.ClaudeAdapter{
		Command:         harnessCommandOrDefault(harnessCommand, string(apiv1.HarnessClaudeCode), []string{"claude"}),
		EnvCapabilities: claudeEnvCaps,
		OptionalCredentialCapabilities: map[string]bool{
			string(capability.AgentModel): true,
		},
		ExtraEnvAllowlist: envPassthrough,
		InstanceRoot:      instanceRoot,
		SelfBin:           selfBin,
	}
	if err := registry.RegisterAs(string(apiv1.HarnessClaudeCode), claudeAdapter); err != nil {
		return nil, fmt.Errorf("register Claude Code harness: %w", err)
	}
	return registry, nil
}

// harnessCommandOrDefault returns the adopter's launcher override for the named
// harness (RunnerConfig.HarnessCommand), or def when unset. It defensively
// copies the override so a later mutation of the config map can't reach into
// the registered adapter, and falls back to def on an empty slice (already
// rejected at config load, but belt-and-suspenders — an empty argv would fail
// at exec).
func harnessCommandOrDefault(overrides map[string][]string, name string, def []string) []string {
	if command, ok := overrides[name]; ok && len(command) > 0 {
		return append([]string(nil), command...)
	}
	return def
}

type deterministicExecutorInput struct {
	Config              *instance.Config
	Resolver            credentials.Resolver
	Grants              []credentials.Grant
	SharedRegistry      *journal.RegistryScrubber
	InstanceRoot        string
	SelfBin             string
	ProjectConfigured   bool
	ConfiguredProject   instance.RepoRef
	GaggleProject       apiv1.RepoRef
	ProviderQuota       *localscheduler.ProviderQuotaState
	ArtifactRecorder    runner.ArtifactRecorder
	SecretRegistrar     runner.SecretRegistrar
	Diagnostics         bool
	DiagnosticsMaxBytes int64
	// ScratchDir, when set, is wired onto the ShellExecutor's own field of the
	// same name (#3342) — the same already-writable scratch directory
	// (runner.Config.ScratchDir, under the instance's workcopies root) the
	// runner uses for scratch-mode workspaces, reused here so the built-in
	// error file never depends on the OS default temp directory being
	// writable under a read-only-root deployment.
	ScratchDir string
}

func buildDeterministicExecutor(input deterministicExecutorInput) (invoke.Deterministic, error) {
	reg := teeRegistrar{run: input.SecretRegistrar, shared: input.SharedRegistry}
	injector, err := credentials.NewInjector(input.Resolver, input.Grants, reg)
	if err != nil {
		return nil, err
	}
	shell, err := executor.NewShellExecutor(injector, input.ArtifactRecorder)
	if err != nil {
		return nil, err
	}
	shell.InstanceRoot = input.InstanceRoot
	shell.ScratchDir = input.ScratchDir
	shell.ExtraEnvAllowlist = input.Config.Runner.EnvPassthrough
	if input.ProjectConfigured && input.ConfiguredProject.LargeRepo {
		shell.DefaultEnv = map[string]string{"MSBUILDDISABLENODEREUSE": "1"}
	}
	defaultStageTimeoutSetting := input.Config.Runner.DefaultStageTimeout
	if input.ProjectConfigured {
		defaultStageTimeoutSetting = input.ConfiguredProject.EffectiveDefaultStageTimeout(defaultStageTimeoutSetting)
	}
	defaultStageTimeout, err := (instance.RunnerConfig{DefaultStageTimeout: defaultStageTimeoutSetting}).DefaultStageTimeoutDuration()
	if err != nil {
		return nil, err
	}
	shell.DefaultTimeout = defaultStageTimeout
	shell.SelfBin = input.SelfBin
	if input.Diagnostics {
		shell.Diagnostics = true
		shell.DefaultMaxOutputBytes = input.DiagnosticsMaxBytes
	}

	kinds := executor.NewKindRegistry()
	if err := kinds.Register(executor.KindShell, shell); err != nil {
		return nil, err
	}
	var adoRepo *instance.RepoRef
	if repo, ok := adoRepoForGaggle(input.Config, input.GaggleProject); ok {
		adoRepo = &repo
	}
	var giteaRepo *instance.RepoRef
	if repo, ok := giteaRepoForGaggle(input.Config, input.GaggleProject); ok {
		giteaRepo = &repo
	}
	ciPoll, err := buildCIPollExecutor(input.Config, injector, input.ArtifactRecorder, adoRepo, giteaRepo, reg, input.ProviderQuota)
	if err != nil {
		return nil, err
	}
	if err := kinds.Register(executor.KindCIPoll, ciPoll); err != nil {
		return nil, err
	}
	telemetryQuery, err := buildExternalTelemetryExecutor(input.Config.ExternalTelemetry, input.ArtifactRecorder, reg)
	if err != nil {
		return nil, err
	}
	if err := kinds.Register(executor.KindExternalTelemetry, telemetryQuery); err != nil {
		return nil, err
	}
	return executor.NewTaskExecutor(kinds)
}

type agenticExecutorInput struct {
	GooberName       string
	Goobers          map[string]apiv1.GooberSpec
	Instructions     map[string]string
	Assets           map[string]*gooberassets.Bundle
	HarnessInfo      harnessPreflightInfo
	AdapterRegistry  *harness.Registry
	EnvCapabilities  map[string]string
	Resolver         credentials.Resolver
	Grants           []credentials.Grant
	SharedRegistry   *journal.RegistryScrubber
	RunsDir          string
	SandboxPosture   instance.SandboxPosture
	ArtifactRecorder runner.ArtifactRecorder
	SecretRegistrar  runner.SecretRegistrar
	AgenticAdapter   func(string, map[string]string) harness.Adapter
}

func buildAgenticExecutor(input agenticExecutorInput) (invoke.Goober, error) {
	spec, ok := input.Goobers[input.GooberName]
	if !ok {
		return nil, fmt.Errorf("goober %q not found in config", input.GooberName)
	}
	harnessName := spec.Harness
	if harnessName == "" {
		harnessName = apiv1.HarnessCopilot
	}
	if err := mcpconfig.ValidateForHarness(harnessName, spec.MCPServers, spec.Capabilities, spec.Tools); err != nil {
		return nil, fmt.Errorf("validate goober %q MCP config: %w", input.GooberName, err)
	}
	credentialKeys := append([]string(nil), spec.Capabilities...)
	credentialKeys = append(credentialKeys, mcpconfig.BYOCredentialKeys(spec.MCPServers)...)
	gooberGrants := buildGooberCredentialGrants(input.GooberName, credentialKeys, input.Grants)
	injector, err := credentials.NewGooberInjectorWithCredentialKeys(
		input.Resolver,
		input.GooberName,
		gooberGrants,
		mcpconfig.BYOCredentialKeys(spec.MCPServers),
		teeRegistrar{run: input.SecretRegistrar, shared: input.SharedRegistry},
	)
	if err != nil {
		return nil, err
	}
	adapter, err := input.AdapterRegistry.Get(string(harnessName))
	if err != nil {
		return nil, fmt.Errorf("resolve goober %q harness: %w", input.GooberName, err)
	}
	if input.AgenticAdapter != nil {
		adapter = input.AgenticAdapter(input.GooberName, input.EnvCapabilities)
	}
	recorder, ok := input.ArtifactRecorder.(harness.SpanRecorder)
	if !ok {
		return nil, fmt.Errorf("runner artifact recorder does not implement harness.SpanRecorder")
	}
	artifacts, ok := input.ArtifactRecorder.(harness.ArtifactRecorder)
	if !ok {
		return nil, fmt.Errorf("runner artifact recorder does not implement harness.ArtifactRecorder")
	}
	direr, ok := input.ArtifactRecorder.(interface{ Dir() string })
	if !ok {
		return nil, fmt.Errorf("runner artifact recorder does not implement Dir() string")
	}
	registryScrubber, ok := input.SecretRegistrar.(journal.Scrubber)
	if !ok {
		return nil, fmt.Errorf("runner secret registrar does not implement journal.Scrubber")
	}
	opts := []harness.Option{
		harness.WithHarnessConfig(spec.Model, spec.HarnessOptions),
		harness.WithHarnessVersion(input.HarnessInfo[harnessName].Version),
		harness.WithAssetBundle(input.Assets[input.GooberName]),
		harness.WithMCPServers(spec.MCPServers),
		harness.WithTools(spec.Tools),
	}
	if spec.TimeoutSeconds > 0 {
		opts = append(opts, harness.WithTimeout(time.Duration(spec.TimeoutSeconds)*time.Second))
	}
	if input.SandboxPosture == instance.SandboxEnforced {
		opts = append(opts, harness.WithSandboxEnforcement())
	}
	return harness.NewExecutor(
		adapter,
		injector,
		recorder,
		artifacts,
		harness.NewContextResolver(direr, input.RunsDir),
		journal.Chain(registryScrubber, journal.NewPatternScrubber()),
		input.Instructions[input.GooberName],
		opts...,
	)
}

// ciPollKindExecutor admits ci-poll's credential against each invocation's
// declared capabilities. Registering it only for KindCIPoll keeps credential
// materialization out of every other deterministic kind.
type ciPollKindExecutor struct {
	injector *credentials.Injector
	recorder executor.ArtifactRecorder
	// adoRepo is set when this gaggle's repo is Azure DevOps. When set, ci-poll
	// builds its poller from instance config (adoauth.Provider shells out to
	// `az` for the token) rather than materializing a GitHub capability token —
	// mirroring the CLI PR stages' provider resolution.
	adoRepo *instance.RepoRef
	// giteaRepo is set when the gaggle's repo is Gitea; ci-poll then builds a
	// Gitea poller from its baseURL + the materialized capability token instead
	// of defaulting to GitHub.
	giteaRepo *instance.RepoRef
	registrar providers.SecretRegistrar
	quota     providers.QuotaObserver
}

func (e *ciPollKindExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	required := string(capability.ProviderPRWrite)
	if !slices.Contains(env.Capabilities, required) {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: kind=%s requires declared capability %q: %w", executor.KindCIPoll, required, credentials.ErrUndeclaredCapability)
	}
	var poller executor.PRPoller
	switch {
	case e.adoRepo != nil:
		provider, err := adoauth.Provider(*e.adoRepo, nil, e.registrar, nil, e.quota, nil)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("build ADO ci-poll provider: %w", err)
		}
		poller = provider
	case e.giteaRepo != nil:
		set, err := e.injector.Materialize(ctx, env.Capabilities)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
		}
		token, err := set.Token(ctx, string(capability.ProviderPRWrite))
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credential: %w", err)
		}
		poller = providers.NewGiteaProvider(e.giteaRepo.BaseURL, token)
	default:
		set, err := e.injector.Materialize(ctx, env.Capabilities)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
		}
		token, err := set.Token(ctx, string(capability.ProviderPRWrite))
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credential: %w", err)
		}
		if newPRPoller != nil {
			poller = newPRPoller(token)
		} else {
			poller = providers.NewGitHubProvider(token)
		}
	}
	ciPoll, err := executor.NewCIPollExecutor(poller, e.recorder)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	pollCfg, err := executor.CIPollConfigFromEnvelope(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return ciPoll.Run(ctx, pollCfg)
}

// buildCIPollExecutor builds the registered ci-poll kind for a repo-backed
// instance. Credential resolution stays lazy until that kind is dispatched.
// When adoRepo is non-nil the gaggle's repo is Azure DevOps, and ci-poll
// resolves its poller from instance config (adoauth.Provider shells out to
// `az` for the token) instead of a GitHub capability token.
func buildCIPollExecutor(cfg *instance.Config, injector *credentials.Injector, recorder executor.ArtifactRecorder, adoRepo *instance.RepoRef, giteaRepo *instance.RepoRef, registrar providers.SecretRegistrar, quota *localscheduler.ProviderQuotaState) (executor.KindExecutor, error) {
	if len(cfg.Repos) == 0 {
		return executor.NewCIPollKindExecutor(nil), nil
	}
	if injector == nil {
		return nil, fmt.Errorf("build ci-poll executor: credential injector is nil")
	}
	if recorder == nil {
		return nil, fmt.Errorf("build ci-poll executor: artifact recorder is nil")
	}
	var quotaObserver providers.QuotaObserver
	if quota != nil {
		quotaObserver = &providerQuotaAccounting{state: quota}
	}
	return &ciPollKindExecutor{injector: injector, recorder: recorder, adoRepo: adoRepo, giteaRepo: giteaRepo, registrar: registrar, quota: quotaObserver}, nil
}

// buildExternalTelemetryExecutor validates every registered plugin
// configuration before a run and constructs the registered query kind.
func buildExternalTelemetryExecutor(
	config externaltelemetry.Configuration,
	recorder executor.ArtifactRecorder,
	registrar externaltelemetry.SecretRegistrar,
) (executor.KindExecutor, error) {
	if recorder == nil {
		return nil, errors.New("build external telemetry executor: artifact recorder is nil")
	}
	registry, err := buildExternalTelemetryRegistry(config, registrar)
	if err != nil {
		return nil, err
	}
	query, err := executor.NewTelemetryQueryExecutor(&externaltelemetry.Host{
		Registry: registry,
	}, recorder)
	if err != nil {
		return nil, err
	}
	return query, nil
}

func buildExternalTelemetryRegistry(
	config externaltelemetry.Configuration,
	registrar externaltelemetry.SecretRegistrar,
) (*externaltelemetry.Registry, error) {
	registry := externaltelemetry.NewRegistry()
	factories := []externaltelemetry.Factory{
		externaltelemetry.FakeFactory{},
		adx.Factory{},
	}
	factories = append(factories, connectorapi.RegisteredFactories()...)
	for _, factory := range factories {
		if err := registry.Register(factory); err != nil {
			return nil, err
		}
	}
	for _, connector := range config.Connectors {
		if err := registry.Configure(connector, nil, registrar); err != nil {
			return nil, err
		}
	}
	return registry, nil
}
