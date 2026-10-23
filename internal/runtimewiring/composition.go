package runtimewiring

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// RunnerCompositionInput supplies shared runtime dependencies and the callbacks
// owned by each binary. ConfigureWorktree installs cleanup guards on both new
// and reused managers; Finalize adds binary-specific runner services.
type RunnerCompositionInput struct {
	// ExecutorBinary names the local CLI used by stage commands and harness MCP servers. Empty uses this process.
	ExecutorBinary           string
	ExecutionFence           ExecutionFenceStart
	Layout                   instance.Layout
	Config                   *instance.Config
	Definitions              *instance.ConfigSet
	Goobers                  map[string]apiv1.GooberSpec
	InstructionsByGoober     map[string]string
	SkillPackages            map[string][]workflow.SkillFile
	Telemetry                *telemetry.Client
	SharedRegistry           *journal.RegistryScrubber
	WorktreeManager          *worktree.Manager
	BranchNamespaces         map[string]string
	GaggleProject            apiv1.RepoRef
	GaggleBacklog            apiv1.BacklogRef
	AdditionalRepos          []apiv1.RepoRef
	HarnessInfo              HarnessPreflightInfo
	CredentialStores         credentials.StoreResolver
	SandboxPosture           instance.SandboxPosture
	ProviderQuota            *localscheduler.ProviderQuotaState
	AppliedConfigDigest      string
	ConfigGeneration         string
	IssueOwnershipAssignees  string
	IssueOwnershipUnassigned string
	Diagnostics              bool
	DiagnosticsMaxBytes      int64
	LookPath                 func(string) (string, error)
	BuildDeterministic       func(DeterministicExecutorInput) (invoke.Deterministic, error)
	BuildAgentic             func(AgenticExecutorInput) (invoke.Goober, error)
	ConfigureWorktree        func(*worktree.Manager) error
	Finalize                 func(*runner.Config, credentials.Resolver, *worktree.Manager) error
}

// BuildRunnerConfig composes shared executors and workspaces with binary-specific hooks.
func BuildRunnerConfig(input RunnerCompositionInput) (runner.Config, *worktree.Manager, error) {
	if input.ExecutionFence == nil {
		return runner.Config{}, nil, fmt.Errorf("runner execution fence is required")
	}
	executionFence := WithCurrentMergeAuthority(input.Layout, input.ExecutionFence)
	if input.LookPath == nil {
		input.LookPath = exec.LookPath
	}
	if input.BuildDeterministic == nil {
		input.BuildDeterministic = BuildDeterministicExecutor
	}
	if input.BuildAgentic == nil {
		input.BuildAgentic = BuildAgenticExecutor
	}
	l := input.Layout
	cfg, goobers := input.Config, input.Goobers
	skillPackages, skillErr := input.resolvedSkillFiles()
	if skillErr != nil {
		return runner.Config{}, nil, skillErr
	}
	tel := input.Telemetry
	sharedReg := input.SharedRegistry
	branchNamespaces := input.BranchNamespaces
	gaggleProject := input.GaggleProject
	additionalRepos := input.AdditionalRepos
	harnessInfo := input.HarnessInfo
	stores := input.CredentialStores
	sandboxPosture := input.SandboxPosture
	providerQuota := input.ProviderQuota
	appliedConfigDigest := input.AppliedConfigDigest
	infra, err := prepareRunnerInfrastructure(input)
	if err != nil {
		return runner.Config{}, nil, err
	}
	resolver, grants := infra.resolver, infra.grants
	deterministicGrants := infra.deterministicGrants
	wtMgr, pathLimits := infra.worktreeManager, infra.pathLimits
	configuredProject, projectConfigured, pinned := infra.configuredProject, infra.projectConfigured, infra.pinned
	if _, err := BuildExternalTelemetryRegistry(cfg.ExternalTelemetry, sharedReg); err != nil {
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
	selfBin := input.ExecutorBinary
	if selfBin == "" {
		selfBin, err = os.Executable()
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("resolve goobers binary path: %w", err)
		}
	}

	envCaps := BuildEnvCapabilities()
	adapterRegistry, err := BuildHarnessRegistry(envCaps, HarnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, instanceRoot, selfBin, false, nil,
		// Same runner property the deterministic executor binds: a self
		// entry declaring tmp:ephemeral must be true of agentic stages too,
		// or the declaration is only half enforced.
		cfg.SelfRunnerEnforces(instance.RunnerRestrictionTmpEphemeral))
	if err != nil {
		return runner.Config{}, nil, err
	}
	assetsByGoober := make(map[string]*gooberassets.Bundle, len(goobers))
	for name, spec := range goobers {
		if _, ok := input.InstructionsByGoober[name]; !ok {
			return runner.Config{}, nil, fmt.Errorf("goober %q has no resolved instructions", name)
		}
		assets, err := gooberassets.Load(filepath.Join(ResolvedGooberDefinitionDir(l.ConfigDir(), input.Definitions, spec, name), gooberassets.SourceDir))
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
	var baselineHealth runner.BaselineHealth
	rc := runner.Config{
		ConfigGeneration: input.ConfigGeneration,
		RunControls:      cfg.RunConditions.RunControls(),
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			exec, err := input.BuildDeterministic(DeterministicExecutorInput{
				Config: cfg, Resolver: resolver, Grants: deterministicGrants, SharedRegistry: sharedReg,
				InstanceRoot: instanceRoot, AppliedConfigDigest: appliedConfigDigest, ConfigDirectory: l.ConfigDir(), SelfBin: selfBin, ProjectConfigured: projectConfigured,
				ConfiguredProject: configuredProject, GaggleProject: gaggleProject, ProviderQuota: providerQuota,
				ArtifactRecorder: rec, SecretRegistrar: reg, Diagnostics: input.Diagnostics, DiagnosticsMaxBytes: input.DiagnosticsMaxBytes,
				ScratchDir: deterministicScratchDir, CredentialStores: stores,
			})
			if err != nil {
				return nil, err
			}
			return ClaimFencedDeterministic{Deterministic: exec, Start: executionFence}, nil
		},
		NewAgenticWithMCPServers: func(gooberName string, servers []apiv1.MCPServer, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			ordinary := func() (invoke.Goober, error) {
				return input.BuildAgentic(AgenticExecutorInput{
					GooberName: gooberName, Goobers: goobers, Instructions: input.InstructionsByGoober, Assets: assetsByGoober, SkillPackages: skillPackages,
					HarnessInfo: harnessInfo, AdapterRegistry: adapterRegistry, EnvCapabilities: envCaps,
					Resolver: resolver, Grants: grants, SharedRegistry: sharedReg, RunsDir: l.RunsDir(),
					SandboxPosture: sandboxPosture, ArtifactRecorder: rec, SecretRegistrar: reg, AgenticAdapter: NewAgenticAdapter,
					GuardedCredentialPaths: instance.GuardedCredentialPaths(cfg),
					MCPServers:             servers, MCPServersSet: servers != nil,
				})
			}
			goober, err := ordinary()
			if err != nil {
				return nil, err
			}
			return ClaimFencedGoober{Goober: goober, Start: executionFence}, nil
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
		BranchNamespaces:    branchNamespaces,
		ScratchDir:          deterministicScratchDir,
		RunsDir:             l.RunsDir(),
		HandoffSchemaLoader: newHandoffSchemaLoader(cfg, l.ConfigDir()),
		RepoCloneURL:        RepoCloneURL,
		// The gaggle's read-only reference repos (MGV-11 #1286): the runner
		// provisions a read-only checkout of each alongside a repo-workspace
		// stage's primary worktree. Empty for a single-repo gaggle (unchanged).
		AdditionalRepos:           additionalRepos,
		ResolveRepositoryIdentity: BuildRevisionIdentityResolver(cfg, resolver, sharedReg, stores),
		GateGooberCapabilities:    gateGooberCaps,
		AgentProvenance:           agentProvenance,
		BaselineHealth:            baselineHealth,
		// PATH-preflight the local-ci stage's configured ciCommand (#1380) for
		// a real daemon run. Left nil in every runner-package test and any
		// embedder that doesn't want it (Config.LookPathFunc's doc comment) —
		// this is the one place that actually wants a host PATH check.
		LookPathFunc: input.LookPath,
	}
	rc.SelfExecutionDenied = cfg.SelfExecutionDenied()
	rc.SelfExecutionObserved = SelfExecutionObserver(cfg, tel)
	applyRunnerConfigFinalizers(&rc, input, tel)
	if input.Finalize != nil {
		if err := input.Finalize(&rc, resolver, wtMgr); err != nil {
			return runner.Config{}, nil, err
		}
	}
	wtMgr.SetPathLengthLimits(pathLimits)
	// Refreshed unconditionally, exactly like the path-length limits above —
	// on BOTH the newly-constructed and the reused-manager path (#4405). A
	// reused Manager's mirror-fetch exclusion previously stayed frozen at
	// whatever namespace was configured when it was first built: a hot
	// config reload that changes spec.branchNamespace never reached it,
	// since WithRunBranchNamespaces above only applies inside the
	// `wtMgr == nil` branch. SetRunBranchNamespaces accumulates rather than
	// replaces, so a run still in flight under the OLD namespace stays
	// protected too — see its own doc comment for why replace-semantics
	// (SetPathLengthLimits's own shape) is the wrong model here.
	wtMgr.SetRunBranchNamespaces(branchNamespaces[l.Gaggle()])
	return withLegacyAgenticFactory(rc), wtMgr, nil
}

type runnerInfrastructure struct {
	resolver            credentials.Resolver
	grants              []credentials.Grant
	deterministicGrants []credentials.Grant
	worktreeManager     *worktree.Manager
	pathLimits          map[string]worktree.PathLengthLimit
	configuredProject   instance.RepoRef
	projectConfigured   bool
	pinned              bool
}

func prepareRunnerInfrastructure(input RunnerCompositionInput) (runnerInfrastructure, error) {
	l := input.Layout
	cfg := input.Config
	tel := input.Telemetry
	sharedReg := input.SharedRegistry
	wtMgr := input.WorktreeManager
	branchNamespaces := input.BranchNamespaces
	gaggleProject := input.GaggleProject
	additionalRepos := input.AdditionalRepos
	stores := input.CredentialStores
	providerQuota := input.ProviderQuota
	// Per-gaggle credential scoping (MGV-5, #1012): this runner serves one
	// gaggle, so its stages are granted that gaggle's own project-repo token —
	// not an instance-wide default. gaggleProject is zero for a single-gaggle /
	// legacy instance, which falls back to the first repo's token unchanged.
	// Computed before the worktree Manager so its per-repo git-auth resolver can
	// back each read-only reference-repo clone with that repo's contents:read
	// token (MGV-10/#1285, consumed by MGV-11/#1286).
	// A backlog on another provider (topology (b)) binds the backlog-family
	// capabilities to the backlog repository's credential.
	resolver, grants, err := BuildGaggleCredentials(cfg, stores, gaggleProject, input.GaggleBacklog, additionalRepos, sharedReg)
	if err != nil {
		return runnerInfrastructure{}, err
	}
	deterministicGrants := DeterministicCredentialGrants(grants)
	// The clone-URL derivation the runner will use (the test seam when set, else
	// the runner default) — the worktree auth resolver must key on the identical
	// URLs the runner hands WorkingCopy.
	cloneURLFn := RepoCloneURL
	if cloneURLFn == nil {
		cloneURLFn = runner.DefaultRepoCloneURL
	}
	pathLimits, pathLimitsErr := PathLengthManagerLimits(cfg, cloneURLFn, runtime.GOOS)
	if pathLimitsErr != nil {
		return runnerInfrastructure{}, pathLimitsErr
	}
	configuredProject, projectConfigured := ConfiguredRepoForProject(cfg, gaggleProject)
	pinned := projectConfigured && configuredProject.Pinned()
	if pinned && len(additionalRepos) > 0 {
		return runnerInfrastructure{}, fmt.Errorf("VER: pinned workspace for %s/%s cannot be combined with additional repository worktrees", gaggleProject.Owner, gaggleProject.Name)
	}
	workcopiesRoot := l.WorkcopiesDir()
	if pinned {
		workcopiesRoot = l.WorkcopiesBaseDir()
	}
	absoluteWorkcopiesRoot, err := filepath.Abs(workcopiesRoot)
	if err != nil {
		return runnerInfrastructure{}, fmt.Errorf("resolve workcopies root: %w", err)
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
			MutationCleanupGuard(l.RunsDir()),
			worktree.WithRunBranchNamespaces(branchNamespaces[l.Gaggle()]),
			worktree.WithPinnedRoot(l.WorkcopiesBaseDir()),
		}
		for repoURL, limit := range pathLimits {
			managerOptions = append(managerOptions, worktree.WithPathLengthLimit(repoURL, limit))
		}
		if gitQuotaGate := AdoRemoteGitQuotaGate(providerQuota); gitQuotaGate != nil {
			managerOptions = append(managerOptions, worktree.WithRemoteGitGate(gitQuotaGate))
		}
		if cfg.PartialCloneEnabled() {
			managerOptions = append(managerOptions, worktree.WithPartialClone())
		}
		if cfg.ObjectCacheEnabled() {
			managerOptions = append(managerOptions, worktree.WithObjectCache())
		}
		gitEnv, gitEnvErr := BuildWorktreeGitEnv(cfg, absoluteWorkcopiesRoot, gaggleProject, additionalRepos, resolver, grants, cloneURLFn, sharedReg, stores)
		if gitEnvErr != nil {
			return runnerInfrastructure{}, gitEnvErr
		}
		if gitEnv != nil {
			managerOptions = append(managerOptions, worktree.WithGitEnvironment(gitEnv))
		}
		if tel != nil {
			managerOptions = append(managerOptions, worktree.WithUsageObserver(l.Gaggle(), tel.RecordWorkcopyUsage))
		}
		wtMgr, err = worktree.NewManager(absoluteWorkcopiesRoot, managerOptions...)
		if err != nil {
			return runnerInfrastructure{}, fmt.Errorf("new worktree manager: %w", err)
		}
	}
	if input.ConfigureWorktree != nil {
		if err := input.ConfigureWorktree(wtMgr); err != nil {
			return runnerInfrastructure{}, err
		}
	}
	return runnerInfrastructure{
		resolver: resolver, grants: grants, deterministicGrants: deterministicGrants,
		worktreeManager: wtMgr, pathLimits: pathLimits,
		configuredProject: configuredProject, projectConfigured: projectConfigured, pinned: pinned,
	}, nil
}

func applyRunnerConfigFinalizers(cfg *runner.Config, input RunnerCompositionInput, tel *telemetry.Client) {
	if tel != nil {
		cfg.Telemetry = tel
	}
	cfg.IssueOwnershipAssignees = input.IssueOwnershipAssignees
	cfg.IssueOwnershipUnassigned = input.IssueOwnershipUnassigned
}

func (input RunnerCompositionInput) resolvedSkillFiles() (map[string][]workflow.SkillFile, error) {
	if input.SkillPackages != nil {
		return input.SkillPackages, nil
	}
	return LoadGooberSkillPackages(input.Layout.ConfigDir(), input.Layout.Gaggle(), input.Goobers)
}
