package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blockerclear"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

type runnerCompositionInput = runtimewiring.ExecutorCompositionInput

var runnerLookPath = exec.LookPath

func buildRunnerConfig(input runnerCompositionInput) (runner.Config, *worktree.Manager, error) {
	l, cfg := input.Layout, input.Config
	instanceRoot, err := filepath.Abs(l.Root)
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve instance root: %w", err)
	}
	if input.ExecutionFence == nil {
		input.ExecutionFence = localSharedExecutionFence(input.Layout)
	}
	decisionObserver := newDecisionShadowObserver(cfg, nil)
	input.Diagnostics, input.DiagnosticsMaxBytes = diagnosticsMode, diagnosticsMaxOutputBytes
	input.LookPath = runnerLookPath
	input.ConfigureWorktree = func(manager *worktree.Manager) error {
		cloneURL := runtimewiring.RepoCloneURL
		if cloneURL == nil {
			cloneURL = runner.DefaultRepoCloneURL
		}
		option, err := recoveryCleanupOption(l, cfg, manager.Root, cloneURL, input.SharedRegistry, input.Telemetry)
		if err != nil {
			return err
		}
		option(manager)
		return nil
	}
	input.BuildDeterministic = func(stage runtimewiring.DeterministicExecutorInput) (invoke.Deterministic, error) {
		stage.CredentialGrants = stageGrantMinterFor(instanceRoot)
		exec, err := buildAutomationDeterministicExecutor(stage)
		if err != nil {
			return nil, err
		}
		return exec, nil
	}
	input.BuildAgentic = func(stage runtimewiring.AgenticExecutorInput) (invoke.Goober, error) {
		stage.Observer = decisionObserver
		stage.ChildWorkflows = childWorkflowAccessFor(instanceRoot, stage.SecretRegistrar)
		ordinary := func() (invoke.Goober, error) { return runtimewiring.BuildAgenticExecutor(stage) }
		return bindParentRouting(instanceRoot, stage.GooberName, stage.ArtifactRecorder, ordinary, stage.Assets[stage.GooberName] != nil)
	}
	var localConfig runner.Config
	input.Finalize = func(shared *runtimewiring.ExecutorConfig, resolver credentials.Resolver, manager *worktree.Manager) error {
		localConfig = localRunnerConfig(*shared)
		rc := &localConfig
		baselineHealth, err := buildBaselineHealth(l, cfg, manager)
		if err != nil {
			return err
		}
		rc.BaselineHealth = baselineHealth
		rc.RecoveryEvents, rc.RecoveryObserved = recoveryRunEvents(l), blockerclear.Reporter(l.SchedulerDir())
		rc.Escalation = buildEscalationNotifier(l, cfg, resolver, input.SharedRegistry)
		rc.ClaimedItems = func(runID string) ([]string, error) { return claimedItemIDsForRun(l, runID) }
		rc.Blocked = buildBlockedHandler(l, cfg, resolver, input.SharedRegistry)
		rc.Failed = buildFailedHandler(l, cfg, resolver, input.SharedRegistry)
		rc.ExistingFix = buildExistingFixHandler(l, cfg, resolver, input.SharedRegistry)
		rc.NotifyTerminal = buildTerminalCircuitBreaker(l, cfg, resolver, input.SharedRegistry, nil)
		*rc = withSelfExecutionPolicy(*rc, cfg, input.Telemetry)
		applyRunnerConfigFinalizers(rc, input, input.Telemetry)
		return nil
	}
	shared, manager, err := runtimewiring.BuildExecutorConfig(input)
	if err != nil {
		return runner.Config{}, nil, err
	}
	// The shared builder installs the legacy adapter after finalization.
	localConfig.NewAgentic = shared.NewAgentic
	return localConfig, manager, nil
}

func applyRunnerConfigFinalizers(cfg *runner.Config, input runnerCompositionInput, tel *telemetry.Client) {
	if tel != nil {
		cfg.Telemetry = tel
	}
	cfg.IssueOwnershipAssignees = input.IssueOwnershipAssignees
	cfg.IssueOwnershipUnassigned = input.IssueOwnershipUnassigned
	if input.Config != nil {
		cfg.ResultHandoffSchemas = input.Config.DecisionGate.ResultSchemas()
	}
	cloneURL := cfg.RepoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	archives := parentArchiveRestorer{layout: input.Layout, config: input.Config, worktrees: cfg.Worktrees, cloneURL: cloneURL, scrubber: input.SharedRegistry}
	forkSources := parallelworkspace.Service{Worktrees: cfg.Worktrees, CloneURL: cloneURL, Policy: func(workspace string) (recovery.SnapshotPolicy, error) {
		return childSnapshotPolicy(workspace, input.Layout.Root, input.Config)
	}}
	cfg.PrepareParentForkSource = func(ctx context.Context, rec runner.OwnedJournalRecorder, request spec.Request, previous *spec.Source) (spec.Source, error) {
		return forkSources.Prepare(ctx, rec, request, previous)
	}
	cfg.PrepareParentForkResult = func(ctx context.Context, rec runner.OwnedJournalRecorder, request spec.ResultRequest, previous *spec.Source) (spec.Source, error) {
		return forkSources.Result(ctx, rec, request, previous)
	}
	cfg.JoinParentFork = func(ctx context.Context, rec runner.OwnedJournalRecorder, request spec.JoinRequest) error {
		return forkSources.Join(ctx, rec, request)
	}
	cfg.RestoreParentArchive = archives.restore
	cfg.RetireParentWorkspaces = archives.retire
}

// resolveWorkflowRunControls collapses one workflow's run-control inheritance
// (#1671) into an effective policy: instance runConditions, then the matched
// repo's override, then the gaggle's spec, then the workflow's own spec.
//
// Every local starter resolves through this one function so a run identity
// never depends on its trigger path.
func resolveWorkflowRunControls(cfg *instance.Config, project apiv1.RepoRef, gaggle apiv1.Gaggle, workflowCfg apiv1.Workflow) (runcontrol.Effective, error) {
	var instanceControls apiv1.RunControls
	if cfg != nil {
		instanceControls = cfg.RunConditions.RunControls()
	}
	if repo, ok := runtimewiring.ConfiguredRepoForProject(cfg, project); ok {
		instanceControls = repo.EffectiveRunControls(instanceControls)
	}
	return runcontrol.Resolve(instanceControls, gaggle.Spec.RunControls, workflowCfg.Spec.RunControls)
}
