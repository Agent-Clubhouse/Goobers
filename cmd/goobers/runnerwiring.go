package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// instructionsPath resolves a goober's Instructions field to an absolute
// file path. Instructions is documented as "relative to the goober
// definition directory" (api/v1alpha1.GooberSpec), which config-as-code
// objects don't retain after instance.LoadConfigDir flattens them into a
// ConfigSet — but every shipped config (internal/instance/starter,
// config-examples/, reference-workflows/) lays goobers out at the same fixed path, so
// that layout convention is reproduced here rather than widening ConfigSet's
// shape for this one field.
func gooberDefinitionDir(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(configDir, "gaggles", spec.Gaggle, "goobers", gooberName)
}

func instructionsPath(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(gooberDefinitionDir(configDir, spec, gooberName), spec.Instructions)
}

func adoRemoteGitQuotaGate(state *localscheduler.ProviderQuotaState) func(context.Context, string) error {
	if state == nil {
		return nil
	}
	return func(_ context.Context, repoURL string) error {
		if !isADORemote(repoURL) {
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

// buildRunnerConfig assembles the runner.Config the daemon (`goobers up`) and
// `goobers run` share: real worktrees, registry-selected harness adapters and
// the shell executor, credentials scoped to instance.yaml's configured repo(s).
// One Config serves every workflow/run — runner.Runner is not bound to a
// single compiled machine. Also returns the *worktree.Manager directly (not
// just embedded in the Config) so the daemon can call Reap on the exact same
// Manager instance the runner itself dispatches through (issue #136) —
// constructing a second, separate Manager over the same root would give Reap
// its own independent repoLocks map, defeating the per-repo git-operation
// serialization both share Root for in the first place.
//
// tel may be nil (instance.yaml's telemetry.enabled: false, issue #129) —
// deliberately NOT assigned to the returned Config.Telemetry field in that
// case. runner.Config.Telemetry is the SpanStarter INTERFACE; a nil
// *telemetry.Client assigned to it would produce a non-nil interface value
// wrapping a nil pointer, so the runner's own `r.cfg.Telemetry == nil` guard
// would incorrectly evaluate false and panic on first use — Go's classic
// typed-nil-in-interface trap. Leaving the field unset keeps the interface
// itself nil.
type runnerCompositionInput struct {
	Layout               instance.Layout
	Config               *instance.Config
	Goobers              map[string]apiv1.GooberSpec
	InstructionsByGoober map[string]string
	Telemetry            *telemetry.Client
	SharedRegistry       *journal.RegistryScrubber
	WorktreeManager      *worktree.Manager
	BranchNamespaces     map[string]string
	GaggleProject        apiv1.RepoRef
	AdditionalRepos      []apiv1.RepoRef
	HarnessInfo          harnessPreflightInfo
	CredentialStores     credentials.StoreResolver
	SandboxPosture       instance.SandboxPosture
	ProviderQuota        *localscheduler.ProviderQuotaState
}

var runnerLookPath = exec.LookPath

func buildRunnerConfig(input runnerCompositionInput) (runner.Config, *worktree.Manager, error) {
	l := input.Layout
	cfg := input.Config
	goobers := input.Goobers
	instructionsByGoober := input.InstructionsByGoober
	tel := input.Telemetry
	sharedReg := input.SharedRegistry
	wtMgr := input.WorktreeManager
	branchNamespaces := input.BranchNamespaces
	gaggleProject := input.GaggleProject
	additionalRepos := input.AdditionalRepos
	harnessInfo := input.HarnessInfo
	stores := input.CredentialStores
	sandboxPosture := input.SandboxPosture
	providerQuota := input.ProviderQuota
	// Per-gaggle credential scoping (MGV-5, #1012): this runner serves one
	// gaggle, so its stages are granted that gaggle's own project-repo token —
	// not an instance-wide default. gaggleProject is zero for a single-gaggle /
	// legacy instance, which falls back to the first repo's token unchanged.
	// Computed before the worktree Manager so its per-repo git-auth resolver can
	// back each read-only reference-repo clone with that repo's contents:read
	// token (MGV-10/#1285, consumed by MGV-11/#1286).
	gaggleOwner := gaggleProject.Owner
	if gaggleProject.Provider == apiv1.ProviderADO && gaggleProject.Project != "" {
		gaggleOwner += "/" + gaggleProject.Project
	}
	resolver, grants, err := buildCredentials(cfg, stores, gaggleOwner, gaggleProject.Name, additionalRepos, sharedReg)
	if err != nil {
		return runner.Config{}, nil, err
	}
	deterministicGrants := deterministicCredentialGrants(grants)
	// The clone-URL derivation the runner will use (the test seam when set, else
	// the runner default) — the worktree auth resolver must key on the identical
	// URLs the runner hands WorkingCopy.
	cloneURLFn := repoCloneURL
	if cloneURLFn == nil {
		cloneURLFn = runner.DefaultRepoCloneURL
	}
	pathLimits, pathLimitsErr := pathLengthManagerLimits(cfg, cloneURLFn, runtime.GOOS)
	if pathLimitsErr != nil {
		return runner.Config{}, nil, pathLimitsErr
	}
	configuredProject, projectConfigured := configuredRepoForProject(cfg, gaggleProject)
	pinned := projectConfigured && configuredProject.Pinned()
	if pinned && len(additionalRepos) > 0 {
		return runner.Config{}, nil, fmt.Errorf("VER: pinned workspace for %s/%s cannot be combined with additional repository worktrees", gaggleProject.Owner, gaggleProject.Name)
	}
	workcopiesRoot := l.WorkcopiesDir()
	if pinned {
		workcopiesRoot = l.WorkcopiesBaseDir()
	}
	absoluteWorkcopiesRoot, err := filepath.Abs(workcopiesRoot)
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve workcopies root: %w", err)
	}
	if wtMgr != nil && wtMgr.Root != absoluteWorkcopiesRoot {
		// A config reload may switch this repo into or out of pinned mode; do
		// not retain a manager rooted in the opposite lifecycle namespace.
		wtMgr = nil
	}
	if wtMgr == nil {
		var err error
		// This layout is gaggle-scoped (l.ForGaggle) in the daemon; its Manager
		// serves only this gaggle's runs, so its mirror-fetch exclusion is
		// seeded with just this gaggle's run-branch namespace. A missing/empty
		// entry leaves the default "goobers/" in place (WithRunBranchNamespaces
		// drops empties), so a single-gaggle default instance is unchanged.
		managerOptions := []worktree.ManagerOption{
			worktree.WithRunBranchNamespaces(branchNamespaces[l.Gaggle()]),
			worktree.WithPinnedRoot(l.WorkcopiesBaseDir()),
		}
		for repoURL, limit := range pathLimits {
			managerOptions = append(managerOptions, worktree.WithPathLengthLimit(repoURL, limit))
		}
		if gitQuotaGate := adoRemoteGitQuotaGate(providerQuota); gitQuotaGate != nil {
			managerOptions = append(managerOptions, worktree.WithRemoteGitGate(gitQuotaGate))
		}
		if cfg.PartialCloneEnabled() {
			managerOptions = append(managerOptions, worktree.WithPartialClone())
		}
		if cfg.ObjectCacheEnabled() {
			managerOptions = append(managerOptions, worktree.WithObjectCache())
		}
		gitEnv, gitEnvErr := buildWorktreeGitEnv(cfg, absoluteWorkcopiesRoot, gaggleProject, additionalRepos, resolver, grants, cloneURLFn, sharedReg, stores)
		if gitEnvErr != nil {
			return runner.Config{}, nil, gitEnvErr
		}
		if gitEnv != nil {
			managerOptions = append(managerOptions, worktree.WithGitEnvironment(gitEnv))
		}
		if tel != nil {
			managerOptions = append(managerOptions, worktree.WithUsageObserver(l.Gaggle(), tel.RecordWorkcopyUsage))
		}
		wtMgr, err = worktree.NewManager(absoluteWorkcopiesRoot, managerOptions...)
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("new worktree manager: %w", err)
		}
	}
	if _, err := buildExternalTelemetryRegistry(cfg.ExternalTelemetry, sharedReg); err != nil {
		return runner.Config{}, nil, fmt.Errorf("preflight external telemetry connectors: %w", err)
	}
	instanceRoot, err := filepath.Abs(l.Root)
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve instance root: %w", err)
	}
	// The running daemon's own binary path, substituted for a bare "goobers"
	// command token in deterministic stages — a fresh stage worktree never
	// contains the goobers binary, so a bare name fails at exec (#229). Fail
	// closed here rather than let every deterministic stage fail at exec time.
	selfBin, err := os.Executable()
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve goobers binary path: %w", err)
	}

	envCaps := buildEnvCapabilities()
	adapterRegistry, err := buildHarnessRegistry(envCaps, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, instanceRoot, selfBin, false, nil,
		// Same runner property the deterministic executor binds: a self
		// entry declaring tmp:ephemeral must be true of agentic stages too,
		// or the declaration is only half enforced.
		cfg.SelfRunnerEnforces(instance.RunnerRestrictionTmpEphemeral))
	if err != nil {
		return runner.Config{}, nil, err
	}
	assetsByGoober := make(map[string]*gooberassets.Bundle, len(goobers))
	for name, spec := range goobers {
		if _, ok := instructionsByGoober[name]; !ok {
			return runner.Config{}, nil, fmt.Errorf("goober %q has no resolved instructions", name)
		}
		assets, err := gooberassets.Load(filepath.Join(gooberDefinitionDir(l.ConfigDir(), spec, name), gooberassets.SourceDir))
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("load goober %q assets: %w", name, err)
		}
		assetsByGoober[name] = assets
	}

	// An agentic gate's reviewer has no stage-level capabilities of its own, so
	// the runner sources them from the reviewer goober's definition (#294). Map
	// each goober to its declared grants for that lookup; only agentic gates
	// consult it (task stages carry their own stage-level capabilities).
	gateGooberCaps := make(map[string][]string, len(goobers))
	agentProvenance := make(map[string]runner.AgentProvenance, len(goobers))
	for name, spec := range goobers {
		if len(spec.Capabilities) > 0 {
			gateGooberCaps[name] = append([]string(nil), spec.Capabilities...)
		}
		harnessName := spec.Harness
		if harnessName == "" {
			harnessName = apiv1.HarnessCopilot
		}
		agentProvenance[name] = runner.AgentProvenance{
			Model:          spec.Model,
			HarnessVersion: harnessInfo[harnessName].Version,
		}
	}

	// Shared with the ShellExecutor's built-in error file (#3342): the same
	// already-writable scratch directory under the instance's workcopies root
	// that scratch-mode deterministic commands use below, so the built-in
	// error file never falls back to the OS default temp directory — which a
	// read-only-root deployment may not have mounted anything writable at.
	deterministicScratchDir := filepath.Join(l.WorkcopiesDir(), "scratch")

	// #2971: compare a failing local-ci stage against the target branch's own
	// health before attributing it to the run. nil unless the instance opted in.
	baselineHealth, err := buildBaselineHealth(l, cfg, wtMgr)
	if err != nil {
		return runner.Config{}, nil, err
	}

	rc := runner.Config{
		RunControls: cfg.RunConditions.RunControls(),
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			return buildDeterministicExecutor(deterministicExecutorInput{
				Config: cfg, Resolver: resolver, Grants: deterministicGrants, SharedRegistry: sharedReg,
				InstanceRoot: instanceRoot, SelfBin: selfBin, ProjectConfigured: projectConfigured,
				ConfiguredProject: configuredProject, GaggleProject: gaggleProject, ProviderQuota: providerQuota,
				ArtifactRecorder: rec, SecretRegistrar: reg, Diagnostics: diagnosticsMode, DiagnosticsMaxBytes: diagnosticsMaxOutputBytes,
				ScratchDir: deterministicScratchDir,
			})
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			return buildAgenticExecutor(agenticExecutorInput{
				GooberName: gooberName, Goobers: goobers, Instructions: instructionsByGoober, Assets: assetsByGoober,
				HarnessInfo: harnessInfo, AdapterRegistry: adapterRegistry, EnvCapabilities: envCaps,
				Resolver: resolver, Grants: grants, SharedRegistry: sharedReg, RunsDir: l.RunsDir(),
				SandboxPosture: sandboxPosture, ArtifactRecorder: rec, SecretRegistrar: reg, AgenticAdapter: newAgenticAdapter,
			})
		},
		Automated: gate.NewAutomatedEvaluator(),
		// Placement provenance is recorded only once this instance declares a
		// runners: inventory (or supplies GOOBERS_RUNNER_* identity env) —
		// zero-declaration installs keep byte-identical journals
		// (goobernetes-architecture.md §11 item 1).
		RunnersDeclared:   len(cfg.Runners) > 0,
		Worktrees:         wtMgr,
		PinnedWorkspace:   pinned,
		PinnedCleanPolicy: configuredProject.WorkspaceCleanPolicy(),
		// Resolve each run's branch namespace from its gaggle (StartInput.Gaggle),
		// so the run branch, the mirror-fetch exclusion above, and the stage
		// env's GOOBERS_BRANCH_NAMESPACE all agree (#965/#1010). Absent/empty
		// entries fall back to providers.DefaultBranchNamespace in the runner.
		BranchNamespaces: branchNamespaces,
		ScratchDir:       deterministicScratchDir,
		RunsDir:          l.RunsDir(),
		RepoCloneURL:     repoCloneURL,
		// The gaggle's read-only reference repos (MGV-11 #1286): the runner
		// provisions a read-only checkout of each alongside a repo-workspace
		// stage's primary worktree. Empty for a single-repo gaggle (unchanged).
		AdditionalRepos:        additionalRepos,
		GateGooberCapabilities: gateGooberCaps,
		AgentProvenance:        agentProvenance,
		BaselineHealth:         baselineHealth,
		// Wire the escalation notifier (#312) so a repass-budget escalation
		// actually comments on the driving issue; nil for a repo-less instance.
		Escalation: buildEscalationNotifier(l, cfg, resolver, sharedReg),
		// Resolve the driving item(s) from the claim ledger when a run has no
		// Item snapshot (#796): scheduled implementation runs self-select their
		// item mid-run, so notifyTerminalGate would otherwise never comment on an
		// escalation. Mirrors the fallback buildBlockedHandler already uses.
		ClaimedItems: func(runID string) ([]string, error) { return claimedItemIDsForRun(l, runID) },
		// Wire the blocked handler (#544/#552): record/park the driving issue
		// when a stage reports blocked; nil for a repo-less instance.
		Blocked: buildBlockedHandler(l, cfg, resolver, sharedReg),
		// Wire the failed handler (#1054): leave a human-visible trace on the
		// driving item when a run ends terminal `failed`, so a recurring infra
		// fault (e.g. a copilot-cli session timeout) stops silently returning the
		// item to ready with no record; nil for a repo-less instance.
		Failed: buildFailedHandler(l, cfg, resolver, sharedReg),
		// Wire the existing-fix handler (#3236): when implement returns no-work
		// with existingFixCommit set, strip goobers:ready to prevent reclaim.
		ExistingFix: buildExistingFixHandler(l, cfg, resolver, sharedReg),
		// Circuit breaker for escalated/aborted terminals: buildFailedHandler
		// covers PhaseFailed; this covers the remaining non-completed terminals
		// so that a repeating escalation loop doesn't churn indefinitely.
		NotifyTerminal: buildTerminalCircuitBreaker(l, cfg, resolver, sharedReg, nil),
		// PATH-preflight the local-ci stage's configured ciCommand (#1380) for
		// a real daemon run. Left nil in every runner-package test and any
		// embedder that doesn't want it (Config.LookPathFunc's doc comment) —
		// this is the one place that actually wants a host PATH check.
		LookPathFunc: runnerLookPath,
	}
	if tel != nil {
		rc.Telemetry = tel
	}
	wtMgr.SetPathLengthLimits(pathLimits)
	return rc, wtMgr, nil
}

func pathLengthManagerLimits(cfg *instance.Config, cloneURL func(apiv1.RepoRef) (string, error), goos string) (map[string]worktree.PathLengthLimit, error) {
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

// resolveWorkflowRunControls collapses one workflow's run-control inheritance
// (#1671) into an effective policy: instance runConditions, then the matched
// repo's override, then the gaggle's spec, then the workflow's own spec.
//
// Every starter must resolve through this one function. The daemon's
// scheduler entry did this inline while `goobers engine-start` did it nowhere,
// so the same workflow pinned a different watchdog budget depending on which
// starter dispatched it (#3820) — a run identity must not depend on that.
func resolveWorkflowRunControls(cfg *instance.Config, project apiv1.RepoRef, gaggle apiv1.Gaggle, workflowCfg apiv1.Workflow) (runcontrol.Effective, error) {
	var instanceControls apiv1.RunControls
	if cfg != nil {
		instanceControls = cfg.RunConditions.RunControls()
	}
	if repo, ok := configuredRepoForProject(cfg, project); ok {
		instanceControls = repo.EffectiveRunControls(instanceControls)
	}
	return runcontrol.Resolve(instanceControls, gaggle.Spec.RunControls, workflowCfg.Spec.RunControls)
}

func configuredRepoForProject(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == apiv1.ProviderADO {
		if repo, ok := adoRepoForGaggle(cfg, project); ok {
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

func adoRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
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

// githubRepoForGaggle is adoRepoForGaggle's GitHub counterpart: the instance
// repo backing this gaggle's project, resolved so its configured token can
// authenticate mirror clone/fetch (#667).
func githubRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "github" {
		return cfg.Repos[0], true
	}
	if project.Provider != apiv1.ProviderGitHub {
		return instance.RepoRef{}, false
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == string(providers.ProviderGitHub) && repo.Owner == project.Owner && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// githubWorktreeGitEnvironment builds the worktree.WithGitEnvironment resolver
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
func githubWorktreeGitEnvironment(workcopiesDir string, repo instance.RepoRef, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (func(context.Context, string) ([]string, error), error) {
	var resolve credentials.ResolveFunc
	switch {
	case repo.GitHubAppAuth():
		// The minting source registers tokens with registrar itself, at mint
		// time — before any consumer (including this one) sees the value.
		mint, err := newGitHubAppTokenSource(repo, registrar, stores)
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

// giteaRepoForGaggle returns the instance Gitea repo config backing a gaggle's
// project repo, mirroring githubRepoForGaggle. A single-repo instance with an
// unspecified project provider resolves to its sole Gitea repo.
func giteaRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "gitea" {
		return cfg.Repos[0], true
	}
	if project.Provider != apiv1.ProviderGitea {
		return instance.RepoRef{}, false
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == string(providers.ProviderGitea) && repo.Owner == project.Owner && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
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

// goobersByName indexes set's Goobers by name for workflow.WithGoobers
// admission and NewAgentic's instructions/harness lookup.
func goobersByName(set *instance.ConfigSet) map[string]apiv1.GooberSpec {
	out := make(map[string]apiv1.GooberSpec, len(set.Goobers))
	for _, g := range set.Goobers {
		out[g.Name] = g.Spec
	}
	return out
}

// knownAutomatedCheckNames returns the automated check names actually
// registered (internal/gate.DefaultChecks()'s keys) for
// workflow.WithKnownChecks — every real automated gate resolves its Check
// against this exact registry (internal/gate.AutomatedEvaluator.Evaluate), so
// a typo here is caught at compile time instead of failing only when a run
// actually reaches that gate (#124).
func knownAutomatedCheckNames() []string {
	checks := gate.DefaultChecks()
	names := make([]string, 0, len(checks))
	for name := range checks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type gooberHarnessConfigError struct {
	Goober string
	Err    error
}

func (e *gooberHarnessConfigError) Error() string {
	return fmt.Sprintf("validate goober %q harness config: %v", e.Goober, e.Err)
}

func (e *gooberHarnessConfigError) Unwrap() error {
	return e.Err
}

type gooberHarnessWarning struct {
	Goober  string
	Warning harness.ConfigWarning
}

type workflowCompileError struct {
	Gaggle   string
	Workflow string
	Err      error
}

func (e *workflowCompileError) Error() string {
	return fmt.Sprintf("compile workflow %q: %v", e.Workflow, e.Err)
}

func (e *workflowCompileError) Unwrap() error {
	return e.Err
}

// compiledMachinesWithWarnings compiles every workflow in set,
// admission-checked against goobers (capabilities, harness, gate-outcome
// coverage, and known automated check names — #124), keyed by gaggle and
// workflow name. WorkflowVersion is registry-assigned (per-name monotonic,
// WF-016); no registry is wired at the instance level yet, so this pins
// version 1 for every workflow, matching run.go's existing limitation until a
// follow-up introduces one.
func compiledMachinesWithWarnings(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, envPassthrough []string, harnessCommand map[string][]string, deferModelDiscovery bool) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[string]apiv1.GooberSpec, []gooberHarnessWarning, error) {
	const workflowVersion = 1
	knownChecks := knownAutomatedCheckNames()
	allowPreview := set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations)
	// The admission registry resolves harness config (model/options), and model
	// resolution spawns the configured launcher for model discovery whenever a
	// goober declares spec.Model — so the launcher override must apply here too,
	// or admission probes the wrong runtime (bare copilot on a wrapper-only
	// host, or a divergent bare install beside the wrapper).
	//
	// It never executes a stage, so it binds no runner restriction.
	adapterRegistry, err := buildHarnessRegistry(nil, envPassthrough, harnessCommand, "", "", deferModelDiscovery, nil, false)
	if err != nil {
		return nil, nil, nil, err
	}
	resolvedGoobers, warnings, err := admitGooberHarnessConfigs(adapterRegistry, goobers)
	if err != nil {
		return nil, nil, nil, err
	}
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
		m, err := workflow.Compile(
			workflow.Definition{
				Name: wf.Name, Version: workflowVersion, DSLVersion: wf.DSLVersion, Spec: wf.Spec,
			},
			workflow.WithGoobers(goobers),
			workflow.WithKnownChecks(knownChecks),
			workflow.WithKnownHarnesses(adapterRegistry.Names()),
			workflow.WithPreviewFeatures(allowPreview),
			workflow.WithGaggleRequiredCapabilities(gaggleRequiredCapabilities[wf.Spec.Gaggle]),
			workflow.WithGaggleRunsOn(gaggleRunsOn[wf.Spec.Gaggle]),
		)
		if err != nil {
			return nil, nil, nil, &workflowCompileError{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name, Err: err}
		}
		machines[localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}] = m
	}
	return machines, resolvedGoobers, warnings, nil
}

func admitGooberHarnessConfigs(adapterRegistry *harness.Registry, goobers map[string]apiv1.GooberSpec) (map[string]apiv1.GooberSpec, []gooberHarnessWarning, error) {
	gooberNames := make([]string, 0, len(goobers))
	for name := range goobers {
		gooberNames = append(gooberNames, name)
	}
	sort.Strings(gooberNames)
	resolvedGoobers := make(map[string]apiv1.GooberSpec, len(goobers))
	var warnings []gooberHarnessWarning
	for _, name := range gooberNames {
		spec := goobers[name]
		harnessName := spec.Harness
		if harnessName == "" {
			harnessName = apiv1.HarnessCopilot
		}
		resolution, err := adapterRegistry.ResolveConfig(string(harnessName), spec.Model, spec.HarnessOptions)
		if err != nil {
			return nil, nil, &gooberHarnessConfigError{Goober: name, Err: err}
		}
		spec.Model = resolution.Model
		spec.HarnessOptions = resolution.HarnessOptions
		resolvedGoobers[name] = spec
		for _, warning := range resolution.Warnings {
			warnings = append(warnings, gooberHarnessWarning{Goober: name, Warning: warning})
		}
		if err := mcpconfig.ValidateForHarness(harnessName, spec.MCPServers, spec.Capabilities, spec.Tools); err != nil {
			return nil, nil, fmt.Errorf("validate goober %q MCP config: %w", name, err)
		}
	}
	return resolvedGoobers, warnings, nil
}

// repoRefsByWorkflow resolves each workflow's RepoRef via its Gaggle's
// declared project (apiv1.GaggleSpec.Project) — a workflow only names its
// gaggle, not a repo directly.
func repoRefsByWorkflow(set *instance.ConfigSet) (map[localscheduler.WorkflowIdentity]apiv1.RepoRef, error) {
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

// sandboxPosturesByGaggle resolves each configured gaggle's effective agentic
// isolation posture (#1305): the gaggle's own sandbox override when declared,
// else the instance-wide sandbox.agentic posture, else disabled. Resolved once
// here, at the composition root, so the per-gaggle runner wiring and anything
// else that needs the posture agree on one resolution (the same shape as
// branchNamespacesByGaggle above).
func sandboxPosturesByGaggle(cfg *instance.Config, set *instance.ConfigSet) map[string]instance.SandboxPosture {
	out := make(map[string]instance.SandboxPosture, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = instance.EffectiveAgenticSandbox(cfg, g)
	}
	return out
}

// branchNamespacesByGaggle maps each configured gaggle to its run-branch
// namespace root (GaggleSpec.BranchNamespace), normalized to a single trailing
// "/" and defaulted to providers.DefaultBranchNamespace when unset. It is the
// one place the gaggle-configured namespace is read for the runtime: the
// per-gaggle worktree Manager's mirror-fetch exclusion (WithRunBranchNamespaces)
// and every run's Runner.Config.BranchNamespaces both derive from it, so the
// branch a run pushes, the exclusion that preserves it, and the PR-selector
// headPrefix all move together instead of drifting off independent literals
// (#965/#1010).
func branchNamespacesByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = providers.NormalizeBranchNamespace(g.Spec.BranchNamespace)
	}
	return out
}

func selfIdentitiesByGaggle(cfg *instance.Config, set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = instance.EffectiveSelfIdentity(cfg, g)
	}
	return out
}

// requireLabelsByGaggle maps each configured gaggle to its comma-joined
// GaggleSpec.RequireLabels default (MIRC-2, #1901) — the same
// gaggle-default shape branchNamespacesByGaggle/selfIdentitiesByGaggle
// resolve, feeding Runner.Config.BacklogQueryRequireLabels so a gaggle
// omitting RequireLabels behaves exactly as before (empty string, a no-op
// in defaultBacklogQueryRequireLabels).
func requireLabelsByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = strings.Join(g.Spec.RequireLabels, ",")
	}
	return out
}
