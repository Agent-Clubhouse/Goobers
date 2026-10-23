package runtimewiring

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/stageexec"
	"github.com/goobers/goobers/providers"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// gooberDefinitionDir is the conventional, name-derived directory of a goober
// definition. It is only the fallback for callers holding no loaded ConfigSet:
// resolvedGooberDefinitionDir prefers the parsed definition file's own
// directory, which validation also resolves instructions and assets against,
// so a goober directory not named after metadata.name behaves the same at
// runtime (#6611).
func gooberDefinitionDir(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	if spec.Gaggle == "" {
		return filepath.Join(filepath.Dir(configDir), "goobers", gooberName)
	}
	return filepath.Join(configDir, "gaggles", spec.Gaggle, "goobers", gooberName)
}

// AdoRemoteGitQuotaGate coordinates remote git requests with the ADO quota authority.
func AdoRemoteGitQuotaGate(state *localscheduler.ProviderQuotaState) func(context.Context, string) error {
	if state == nil {
		return nil
	}
	return func(_ context.Context, repoURL string) error {
		if !providers.IsADORemoteURL(repoURL) {
			return nil
		}
		decision := state.ReservePolls(apiv1.ProviderADO, time.Now(), 1)
		if decision.Allowed != 0 {
			return nil
		}
		return &localscheduler.ProviderPollBudgetError{
			Provider:  decision.Provider,
			Remaining: decision.RemainingBefore,
			Requested: 1,
			ResetAt:   decision.ResetAt,
		}
	}
}

func withLegacyAgenticFactory(config ExecutorConfig) ExecutorConfig {
	config.NewAgentic = legacyAgenticFactory(config.NewAgenticWithMCPServers)
	return config
}

func legacyAgenticFactory(next stageexec.NewAgenticWithMCPServersFunc) stageexec.NewAgenticFunc {
	return func(name string, rec stageexec.ArtifactRecorder, reg stageexec.SecretRegistrar) (invoke.Goober, error) {
		return next(name, nil, rec, reg)
	}
}

// PathLengthManagerLimits derives portable workspace path limits from runner configuration.
func PathLengthManagerLimits(cfg *instance.Config, cloneURL func(apiv1.RepoRef) (string, error), goos string) (map[string]worktree.PathLengthLimit, error) {
	limits := make(map[string]worktree.PathLengthLimit)
	for i, repo := range cfg.Repos {
		if repo.PathLength != nil && repo.PathLength.Disabled {
			continue
		}
		if repo.PathLength == nil && goos != "windows" {
			continue
		}
		url, err := cloneURL(apiv1.RepoRef{
			Provider: apiv1.Provider(repo.Provider),
			BaseURL:  repo.BaseURL,
			Owner:    repo.Owner,
			Project:  repo.Project,
			Name:     repo.Name,
		})
		if err != nil {
			return nil, fmt.Errorf("repos[%d] (%s/%s): resolve clone URL for path-length preflight: %w", i, repo.Owner, repo.Name, err)
		}
		limit := worktree.PathLengthLimit{MaxPathLength: worktree.DefaultMaxPathLength}
		if repo.PathLength != nil {
			if repo.PathLength.MaxPathLength != 0 {
				limit.MaxPathLength = repo.PathLength.MaxPathLength
			}
			limit.BuildOutputAllowance = repo.PathLength.BuildOutputAllowance
		}
		limits[url] = limit
	}
	return limits, nil
}

// ConfiguredRepoForProject finds the configured repository matching a project reference.
func ConfiguredRepoForProject(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == apiv1.ProviderADO {
		if repo, ok := AdoRepoForGaggle(cfg, project); ok {
			return repo, true
		}
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == string(project.Provider) && repo.Owner == project.Owner &&
			repo.Project == project.Project && repo.Name == project.Name {
			return repo, true
		}
	}
	if len(cfg.Repos) == 1 && project.Owner == "" && project.Name == "" {
		return cfg.Repos[0], true
	}
	return instance.RepoRef{}, false
}

// AdoRepoForGaggle selects the ADO repository used by a gaggle.
func AdoRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "ado" {
		return cfg.Repos[0], true
	}
	if project.Provider != apiv1.ProviderADO {
		return instance.RepoRef{}, false
	}
	organization := project.Owner
	projectName := project.Project
	if projectName == "" {
		organization, projectName, _ = strings.Cut(project.Owner, "/")
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == string(providers.ProviderADO) && repo.Owner == organization && repo.Project == projectName && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// GithubRepoForGaggle is adoRepoForGaggle's GitHub counterpart: the instance
// repo backing this gaggle's project, resolved so its configured token can
// authenticate mirror clone/fetch (#667).
func GithubRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	return repoForGaggleProvider(cfg, project, apiv1.ProviderGitHub, string(providers.ProviderGitHub))
}

func repoForGaggleProvider(cfg *instance.Config, project apiv1.RepoRef, apiProvider apiv1.Provider, instanceProvider string) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == instanceProvider {
		return cfg.Repos[0], true
	}
	if project.Provider != apiProvider {
		return instance.RepoRef{}, false
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == instanceProvider && repo.Owner == project.Owner && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// GithubWorktreeGitEnvironment builds the worktree.WithGitEnvironment resolver
// that authenticates mirror clone/fetch of a GitHub repo with its configured
// credential (#667), via the secret-free askpass helper — the token only ever
// exists in the git child process's environment, never on disk or argv.
//
// A repo with no credential returns a nil resolver and writes nothing: a
// public-repo instance keeps today's unauthenticated child environment, byte
// for byte. With a token ref configured the resolver re-resolves it on every
// clone/fetch (rotation without restart, matching the env/file resolver's
// contract); a github-app repo (#686) mints per operation instead, so a
// refreshed installation token flows into the next fetch with no worktree
// changes. A store-backed token ref (#683) resolves through stores like
// env/file refs. All three fail closed — an unresolvable ref or failed mint
// aborts provisioning rather than falling back to an anonymous fetch, and
// GIT_TERMINAL_PROMPT=0 turns a rejected credential into an immediate error
// instead of an interactive hang. The token is scoped to the configured repo:
// any other remote URL the manager is ever pointed at gets the ambient
// (unauthenticated) environment.
func GithubWorktreeGitEnvironment(workcopiesDir string, repo instance.RepoRef, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (func(context.Context, string) ([]string, error), error) {
	var resolve credentials.ResolveFunc
	switch {
	case repo.GitHubAppAuth():
		// The minting source registers tokens with registrar itself, at mint
		// time — before any consumer (including this one) sees the value.
		mint, err := NewGitHubAppTokenSource(repo, registrar, stores)
		if err != nil {
			return nil, err
		}
		resolve = mint.DropExpiry()
	case repo.Token.Configured():
		// A static token ref (env|file|store) resolves through stores; a
		// store-backed ref can never fall into the unauthenticated arm because
		// Configured() counts it as a source and resolver construction fails
		// closed without store support.
		refName := repo.Owner + "/" + repo.Name
		resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{repo.Token.CredentialTokenRef(refName)}, stores)
		if err != nil {
			return nil, err
		}
		resolve = func(ctx context.Context) (string, error) {
			return resolver.Resolve(ctx, refName)
		}
	default:
		return nil, nil
	}
	askpass, err := credentials.WriteAskpassScript(filepath.Join(workcopiesDir, "auth"))
	if err != nil {
		return nil, err
	}
	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", repo.Owner, repo.Name)
	return func(ctx context.Context, repoURL string) ([]string, error) {
		if !sameGitRemote(repoURL, cloneURL) {
			return nil, nil
		}
		token, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		if registrar != nil {
			registrar.Register([]byte(token))
		}
		return credentials.GitAuthEnvironment(askpass, token), nil
	}, nil
}

// GiteaRepoForGaggle returns the instance Gitea repo config backing a gaggle's
// project repo, mirroring githubRepoForGaggle. A single-repo instance with an
// unspecified project provider resolves to its sole Gitea repo.
func GiteaRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	return repoForGaggleProvider(cfg, project, apiv1.ProviderGitea, string(providers.ProviderGitea))
}

// giteaWorktreeGitEnvironment builds the worktree.WithGitEnvironment resolver
// that authenticates mirror clone/fetch and run-branch push of a Gitea repo
// with its configured token, scoped to the repo's smart-HTTP clone URL
// (<baseURL>/<owner>/<name>.git — the same URL defaultRepoCloneURL derives).
// Gitea has no app-install auth, so only a static/store-backed token applies;
// returns (nil, nil) when the repo carries no configured token (public read,
// ambient). The token is resolved per call and never persisted.
func giteaWorktreeGitEnvironment(repo instance.RepoRef, registrar providers.SecretRegistrar, stores credentials.StoreResolver) (func(context.Context, string) ([]string, error), error) {
	if !repo.Token.Configured() {
		return nil, nil
	}
	base := strings.TrimSuffix(strings.TrimSpace(repo.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("gitea repo %s/%s requires baseUrl for worktree authentication", repo.Owner, repo.Name)
	}
	refName := repo.Owner + "/" + repo.Name
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{repo.Token.CredentialTokenRef(refName)}, stores)
	if err != nil {
		return nil, err
	}
	cloneURL := fmt.Sprintf("%s/%s/%s.git", base, repo.Owner, repo.Name)
	return func(ctx context.Context, repoURL string) ([]string, error) {
		if !sameGitRemote(repoURL, cloneURL) {
			return nil, nil
		}
		token, err := resolver.Resolve(ctx, refName)
		if err != nil {
			return nil, err
		}
		return providers.GiteaGitAuthEnvironment(token, repoURL, registrar), nil
	}, nil
}

// sameGitRemote reports whether two https remote URLs name the same repo,
// tolerating the cosmetic variance git remotes carry: an optional .git
// suffix, a trailing slash, and case (GitHub owner/name are case-insensitive).
func sameGitRemote(a, b string) bool {
	normalize := func(u string) string {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		u = strings.TrimSuffix(u, ".git")
		return strings.ToLower(u)
	}
	return normalize(a) == normalize(b)
}

// GoobersByName indexes set's Goobers by name for workflow.WithGoobers
// admission and NewAgentic's instructions/harness lookup.
func GoobersByName(set *instance.ConfigSet) map[string]apiv1.GooberSpec {
	out := make(map[string]apiv1.GooberSpec, len(set.Goobers))
	for _, g := range set.Goobers {
		out[g.Name] = g.Spec
	}
	return out
}

// KnownAutomatedCheckNames returns the automated check names actually
// registered (internal/gate.DefaultChecks()'s keys) for
// workflow.WithKnownChecks — every real automated gate resolves its Check
// against this exact registry (internal/gate.AutomatedEvaluator.Evaluate), so
// a typo here is caught at compile time instead of failing only when a run
// actually reaches that gate (#124).
func KnownAutomatedCheckNames() []string {
	checks := gate.DefaultChecks()
	names := make([]string, 0, len(checks))
	for name := range checks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GooberHarnessConfigError identifies an invalid harness configuration for a goober.
type GooberHarnessConfigError struct {
	Goober string
	Err    error
}

func (e *GooberHarnessConfigError) Error() string {
	return fmt.Sprintf("validate goober %q harness config: %v", e.Goober, e.Err)
}

func (e *GooberHarnessConfigError) Unwrap() error {
	return e.Err
}

// GooberHarnessWarning associates an admission warning with a goober.
type GooberHarnessWarning struct {
	Goober  string
	Warning harness.ConfigWarning
}

// WorkflowCompileError identifies the workflow that could not be compiled.
type WorkflowCompileError struct {
	Gaggle   string
	Workflow string
	Err      error
}

func (e *WorkflowCompileError) Error() string {
	return fmt.Sprintf("compile workflow %q: %v", e.Workflow, e.Err)
}

func (e *WorkflowCompileError) Unwrap() error {
	return e.Err
}

// CompiledMachinesWithWarnings compiles every workflow in set,
// admission-checked against goobers (capabilities, harness, gate-outcome
// coverage, and known automated check names — #124), keyed by gaggle and
// workflow name. WorkflowVersion is registry-assigned (per-name monotonic,
// WF-016); no registry is wired at the instance level yet, so this pins
// version 1 for every workflow, matching run.go's existing limitation until a
// follow-up introduces one.
//
// knownTelemetryConnectors (#4475) is the instance's configured
// external-telemetry connector names (instance.Config.ExternalTelemetryConnectorNames).
// Non-nil — even empty — rejects any task whose inputs.connector names a
// connector the instance does not configure, at compile time rather than when
// a run reaches the stage. Nil skips the check, for callers that compile
// without the instance config's authority over connectors.
func CompiledMachinesWithWarnings(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, environment harness.EnvironmentConfig, harnessCommand map[string][]string, deferModelDiscovery bool, modelCredential func(ctx context.Context) (string, error), knownTelemetryConnectors []string) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[string]apiv1.GooberSpec, []GooberHarnessWarning, error) {
	// The admission registry resolves harness config (model/options), and model
	// resolution spawns the configured launcher for model discovery whenever a
	// goober declares spec.Model — so the launcher override must apply here too,
	// or admission probes the wrong runtime (bare copilot on a wrapper-only
	// host, or a divergent bare install beside the wrapper).
	//
	// modelCredential (#4292) is the same instance-configured agent:model
	// resolver the daemon-startup harness preflight already uses — passing nil
	// here left admission-time model discovery with no way to see a
	// file/keychain/store-sourced credential, so every deployment had to also
	// deliver the token as a raw ambient env var just to satisfy discovery.
	//
	// It never executes a stage, so it binds no runner restriction.
	adapterRegistry, err := BuildHarnessRegistry(nil, environment, harnessCommand, "", "", deferModelDiscovery, modelCredential, false)
	if err != nil {
		return nil, nil, nil, err
	}
	resolvedGoobers, warnings, err := AdmitGooberHarnessConfigs(adapterRegistry, goobers)
	if err != nil {
		return nil, nil, nil, err
	}
	machines, err := CompileWorkflowMachines(set, goobers, adapterRegistry.Names(), knownTelemetryConnectors)
	if err != nil {
		return nil, nil, nil, err
	}
	return machines, resolvedGoobers, warnings, nil
}

// CompileWorkflowMachines applies structural workflow admission independently
// of harness model/options admission, so diagnostics can report invalid harness
// configuration without inventing a different workflow compilation path.
func CompileWorkflowMachines(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, harnessNames, knownTelemetryConnectors []string) (map[localscheduler.WorkflowIdentity]*workflow.Machine, error) {
	const workflowVersion = 1
	knownChecks := KnownAutomatedCheckNames()
	// Gaggle-level runner requirements feed push-boundary admission (#2861):
	// each stage's effective requirement set is its gaggle's
	// RequiredCapabilities union its own. The DSL 3.0 successor surface — the
	// gaggle runsOn floor — feeds the 3.0 interpreter's merge rule the same
	// way (dsl-3.0.md §2), and pairing it with a pre-3.0 workflow is a
	// compile error the router raises.
	gaggleRequiredCapabilities := make(map[string][]string, len(set.Gaggles))
	gaggleRunsOn := make(map[string]*apiv1.GaggleRunsOn, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleRequiredCapabilities[set.Gaggles[i].Name] = set.Gaggles[i].Spec.RequiredCapabilities
		gaggleRunsOn[set.Gaggles[i].Name] = set.Gaggles[i].Spec.RunsOn
	}
	machines := make(map[localscheduler.WorkflowIdentity]*workflow.Machine, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		// Preview authorization is per-Workflow (#4220): wf's OWN annotations,
		// never the Manifest's or its gaggle's.
		opts := []workflow.Option{
			workflow.WithGoobers(goobers),
			workflow.WithKnownChecks(knownChecks),
			workflow.WithKnownHarnesses(harnessNames),
			workflow.WithPreviewFeatures(workflow.PreviewFeaturesEnabled(wf.Annotations)),
			workflow.WithGaggleRequiredCapabilities(gaggleRequiredCapabilities[wf.Spec.Gaggle]),
			workflow.WithGaggleRunsOn(gaggleRunsOn[wf.Spec.Gaggle]),
		}
		if knownTelemetryConnectors != nil {
			opts = append(opts, workflow.WithKnownExternalTelemetryConnectors(knownTelemetryConnectors))
		}
		m, err := workflow.Compile(
			workflow.Definition{
				Name: wf.Name, Version: workflowVersion, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations,
			},
			opts...,
		)
		if err != nil {
			return nil, &WorkflowCompileError{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name, Err: err}
		}
		machines[localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}] = m
	}
	return machines, nil
}

// AdmitGooberHarnessConfigs validates goober harness configuration before dispatch.
func AdmitGooberHarnessConfigs(adapterRegistry *harness.Registry, goobers map[string]apiv1.GooberSpec) (map[string]apiv1.GooberSpec, []GooberHarnessWarning, error) {
	gooberNames := make([]string, 0, len(goobers))
	for name := range goobers {
		gooberNames = append(gooberNames, name)
	}
	sort.Strings(gooberNames)
	resolvedGoobers := make(map[string]apiv1.GooberSpec, len(goobers))
	var warnings []GooberHarnessWarning
	for _, name := range gooberNames {
		spec := goobers[name]
		harnessName := spec.Harness
		if harnessName == "" {
			harnessName = apiv1.HarnessCopilot
		}
		resolution, err := adapterRegistry.ResolveConfig(string(harnessName), spec.Model, spec.HarnessOptions)
		if err != nil {
			return nil, nil, &GooberHarnessConfigError{Goober: name, Err: err}
		}
		spec.Model = resolution.Model
		spec.HarnessOptions = resolution.HarnessOptions
		resolvedGoobers[name] = spec
		for _, warning := range resolution.Warnings {
			warnings = append(warnings, GooberHarnessWarning{Goober: name, Warning: warning})
		}
		if err := mcpconfig.ValidateForHarness(harnessName, spec.MCPServers, spec.Capabilities, spec.Tools); err != nil {
			return nil, nil, fmt.Errorf("validate goober %q MCP config: %w", name, err)
		}
	}
	return resolvedGoobers, warnings, nil
}

// RepoRefsByWorkflow resolves each workflow's RepoRef via its Gaggle's
// declared project (apiv1.GaggleSpec.Project) — a workflow only names its
// gaggle, not a repo directly.
func RepoRefsByWorkflow(set *instance.ConfigSet) (map[localscheduler.WorkflowIdentity]apiv1.RepoRef, error) {
	gagglesByName := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for _, g := range set.Gaggles {
		gagglesByName[g.Name] = g
	}
	refs := make(map[localscheduler.WorkflowIdentity]apiv1.RepoRef, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		g, ok := gagglesByName[wf.Spec.Gaggle]
		if !ok {
			return nil, fmt.Errorf("workflow %q references unknown gaggle %q", wf.Name, wf.Spec.Gaggle)
		}
		refs[localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}] = g.Spec.Project
	}
	return refs, nil
}

// SandboxPosturesByGaggle resolves each configured gaggle's effective agentic
// isolation posture (#1305): the gaggle's own sandbox override when declared,
// else the instance-wide sandbox.agentic posture, else disabled. Resolved once
// here, at the composition root, so the per-gaggle runner wiring and anything
// else that needs the posture agree on one resolution (the same shape as
// branchNamespacesByGaggle above).
func SandboxPosturesByGaggle(cfg *instance.Config, set *instance.ConfigSet) map[string]instance.SandboxPosture {
	out := make(map[string]instance.SandboxPosture, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = instance.EffectiveAgenticSandbox(cfg, g)
	}
	return out
}

// BranchNamespacesByGaggle maps each configured gaggle to its run-branch
// namespace root (GaggleSpec.BranchNamespace), normalized to a single trailing
// "/" and defaulted to providers.DefaultBranchNamespace when unset. It is the
// one place the gaggle-configured namespace is read for the runtime: the
// per-gaggle worktree Manager's mirror-fetch exclusion (WithRunBranchNamespaces)
// and every run's Runner.Config.BranchNamespaces both derive from it, so the
// branch a run pushes, the exclusion that preserves it, and the PR-selector
// headPrefix all move together instead of drifting off independent literals
// (#965/#1010).
func BranchNamespacesByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = providers.NormalizeBranchNamespace(g.Spec.BranchNamespace)
	}
	return out
}

// SelfIdentitiesByGaggle resolves provider identities for each gaggle.
func SelfIdentitiesByGaggle(cfg *instance.Config, set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = instance.EffectiveSelfIdentity(cfg, g)
	}
	return out
}

// RequireLabelsByGaggle maps each configured gaggle to its comma-joined
// GaggleSpec.RequireLabels default (MIRC-2, #1901) — the same
// gaggle-default shape branchNamespacesByGaggle/selfIdentitiesByGaggle
// resolve, feeding Runner.Config.BacklogQueryRequireLabels so a gaggle
// omitting RequireLabels behaves exactly as before (empty string, a no-op
// in defaultBacklogQueryRequireLabels).
func RequireLabelsByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = strings.Join(g.Spec.RequireLabels, ",")
	}
	return out
}

// IssueOwnershipAssigneesByGaggle resolves the ownership assignees for each gaggle.
func IssueOwnershipAssigneesByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Spec.IssueOwnershipScope != nil {
			out[g.Name] = strings.Join(g.Spec.IssueOwnershipScope.Assignees, ",")
		}
	}
	return out
}

// IssueOwnershipUnassignedByGaggle resolves unassigned-issue ownership policy for each gaggle.
func IssueOwnershipUnassignedByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Spec.IssueOwnershipScope != nil {
			out[g.Name] = g.Spec.IssueOwnershipScope.Unassigned
		}
	}
	return out
}

type issueOwnershipDefaults struct {
	Assignees  map[string]string
	Unassigned map[string]string
}

// IssueOwnershipDefaultsByGaggle resolves inherited ownership policy for each gaggle.
func IssueOwnershipDefaultsByGaggle(set *instance.ConfigSet) issueOwnershipDefaults {
	return issueOwnershipDefaults{
		Assignees:  IssueOwnershipAssigneesByGaggle(set),
		Unassigned: IssueOwnershipUnassignedByGaggle(set),
	}
}

// BacklogLabelsByGaggle indexes backlog label filters by gaggle.
func BacklogLabelsByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = strings.Join(g.Spec.Backlog.Labels, ",")
	}
	return out
}

// BacklogLabelPredicatesByGaggle indexes backlog label predicates by gaggle.
func BacklogLabelPredicatesByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = g.Spec.Backlog.LabelPredicate
	}
	return out
}
