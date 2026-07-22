package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// schedulerSetup bundles everything both `up` and `run` need to build a
// localscheduler.Scheduler over an instance's config: per-gaggle runners and
// worktree managers, the
// telemetry client both the runner and the scheduler span through, the
// telemetry rollup every dispatched run incrementally ingests into (issue
// #127), the instance
// log, and one WorkflowEntry per configured workflow. Factored out so both
// commands construct it identically (issue #134: `run` used to build its own
// bare *runner.Runner and skip the scheduler/conditions/journal/lock
// entirely — the two commands must agree on this construction, not maintain
// two divergent copies of it). The caller owns calling Telemetry.Shutdown and
// RollupDB.Close once it's done driving runs, exactly as it did before this
// seam existed.
type schedulerSetup struct {
	Runner            *runner.Runner
	Runners           map[string]*runner.Runner
	LegacyRunner      *runner.Runner
	Telemetry         *telemetry.Client
	RollupDB          *rollup.DB
	Config            *instance.Config
	Definitions       *instance.ConfigSet
	Worktrees         *worktree.Manager
	WorktreesByGaggle map[string]*worktree.Manager
	LegacyWorktrees   *worktree.Manager
	InstanceLog       *journal.InstanceLog
	Entries           []localscheduler.WorkflowEntry
	Machines          map[localscheduler.WorkflowIdentity]*workflow.Machine
	RepoRefs          map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	RunConditions     instance.RunConditions
	Validation        *validate.Report
	ConfigDigest      string
	RecoveredClaims   []localscheduler.ClaimEntry
	// OpenPRRefresher backs the #353 MaxOpenPRs cap; nil when no workflow opts
	// in (or no repo is configured). Only the `up` daemon starts its Run loop
	// and wires it as a scheduler option — see up.go.
	OpenPRRefresher *localscheduler.OpenPRRefresher
	// ProviderQuota backs the #712 provider-quota circuit breaker: shared by
	// pointer with runnerCfg.RateLimited (buildRateLimitedHandler), which
	// writes to it the instant a stage fails with providers.ErrorCodeRateLimited;
	// SchedulerOptions wires the same pointer into the Scheduler's Admit check.
	// Unlike OpenPRRefresher this needs no background Run loop (it's pushed to,
	// not polled), so it's wired uniformly for both `up` and `run` in
	// SchedulerOptions rather than needing an up.go-only branch. Never nil.
	ProviderQuota    *localscheduler.ProviderQuotaState
	SharedRegistry   *journal.RegistryScrubber
	TerminalNotifier runner.TerminalNotifier
	RunnerRegistry   *daemonRunnerRegistry
}

type schedulerDefinitions struct {
	Set               *instance.ConfigSet
	Validation        *validate.Report
	HarnessPreflight  harnessPreflightInfo
	Runner            *runner.Runner
	Runners           map[string]*runner.Runner
	Entries           []localscheduler.WorkflowEntry
	Machines          map[localscheduler.WorkflowIdentity]*workflow.Machine
	RepoRefs          map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	OpenPRRefresher   *localscheduler.OpenPRRefresher
	Worktrees         *worktree.Manager
	WorktreesByGaggle map[string]*worktree.Manager
}

// buildSchedulerSetup loads an instance's config, compiles its workflows,
// resolves their RepoRefs, constructs the per-gaggle runners, telemetry client,
// and telemetry rollup, and builds one localscheduler.WorkflowEntry per
// workflow — everything localscheduler.New needs. wg is threaded into every
// entry's trackedStarter so a caller (up's daemon loop, or run's single
// foreground trigger) can track dispatched runs uniformly.
func buildSchedulerSetup(ctx context.Context, l instance.Layout, wg *sync.WaitGroup, setupOpts ...schedulerSetupOption) (_ *schedulerSetup, err error) {
	return buildSchedulerSetupWithConfigPolicy(ctx, l, wg, false, setupOpts...)
}

func buildSchedulerSetupAllowingInvalidConfig(ctx context.Context, l instance.Layout, wg *sync.WaitGroup, setupOpts ...schedulerSetupOption) (_ *schedulerSetup, err error) {
	return buildSchedulerSetupWithConfigPolicy(ctx, l, wg, true, setupOpts...)
}

func buildSchedulerSetupWithConfigPolicy(ctx context.Context, l instance.Layout, wg *sync.WaitGroup, allowInvalidConfig bool, setupOpts ...schedulerSetupOption) (_ *schedulerSetup, err error) {
	var options schedulerSetupOptions
	for _, apply := range setupOpts {
		apply(&options)
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return nil, err
	}
	configDigest, err := configDirectoryDigest(l.ConfigDir())
	if err != nil {
		return nil, err
	}
	configLoader := loadConfigDirectory
	if allowInvalidConfig {
		configLoader = instance.LoadConfigDirForComparison
	}
	set, report, err := configLoader(l.ConfigDir())
	if err != nil {
		if !allowInvalidConfig || !errors.Is(err, instance.ErrInvalidConfig) || set == nil {
			return nil, &configReportError{
				report: report,
				err:    fmt.Errorf("config directory invalid: %w", err),
			}
		}
		err = nil
	}
	defer func() {
		if err != nil {
			err = &configReportError{report: report, err: err}
		}
	}()
	// MGV-1/#1009: resolve each gaggle's declared CI command into its local-ci
	// stage before the workflows are compiled, so the runner executes the
	// gaggle's own suite in place of the stage's declared `make ci` default.
	instance.ApplyGaggleCICommand(set)
	// RRQ-1/#1101: fail closed at startup when a gaggle/stage requires a runner
	// capability the runner (instance.yaml runner.capabilities) does not claim,
	// rather than letting every schedule tick refuse the run at runtime.
	if err := instance.CheckCapabilityRequirements(cfg.Runner.Capabilities, set); err != nil {
		return nil, err
	}
	gaggles := configuredGaggleNames(set)
	if err := l.MigrateLegacyRuntime(gaggles); err != nil {
		return nil, err
	}
	claimProviders := claimProvidersByGaggle(set)

	// telemetry.enabled defaults to true; instance.yaml can opt out (issue
	// #129). tel/rollupDB stay nil in that case — every downstream use
	// already tolerates nil: buildRunnerConfig only sets
	// runner.Config.Telemetry when tel != nil, ingestRunTelemetry no-ops on a
	// nil *rollup.DB, and SchedulerOptions/Shutdown below no-op too. A nil
	// *telemetry.Client must never reach localscheduler.WithTelemetry
	// directly — that would wrap it in a non-nil SpanStarter interface value
	// (Go's typed-nil-in-interface trap), making localscheduler's own
	// `s.telemetry == nil` guard wrongly evaluate false and panic on first
	// use; SchedulerOptions is the one place that decision is made.
	// One instance-global registry, fed by every run's resolved credentials (via
	// the teeRegistrar in buildRunnerConfig) and chained before the pattern net.
	// It is what lets the span exporter and instance log — both instance-lifetime,
	// outliving any single run — redact resolver-issued secrets by exact value,
	// not just by shape (#117 Piece B). Registry redaction is concurrent-safe and
	// keyed by digest, so many runs feeding it is fine.
	sharedReg := journal.NewRegistryScrubber()
	sharedScrubber := journal.Chain(sharedReg, journal.NewPatternScrubber())
	terminalNotifier := buildTerminalNotifier(l, cfg, sharedScrubber, options)

	var tel *telemetry.Client
	var rollupDB *rollup.DB
	if cfg.TelemetryEnabled() {
		var otlpConfig instance.OTLPConfig
		if cfg.Telemetry.OTLP != nil {
			otlpConfig = *cfg.Telemetry.OTLP
		}
		tel, err = buildTelemetryClient(ctx, l, sharedScrubber, sharedReg, otlpConfig)
		if err != nil {
			return nil, err
		}
		rollupDB, err = rollup.Open(l.TelemetryDB())
		if err != nil {
			return nil, err
		}
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir(), journal.WithScrubber(sharedScrubber))
	if err != nil {
		return nil, fmt.Errorf("open instance log: %w", err)
	}
	var recoveredClaims []localscheduler.ClaimEntry
	if err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationMigration, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(l.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(instanceLog),
		)
		if err != nil {
			return err
		}
		recoveredClaims, err = ledger.RecoverExpired(time.Now())
		if err != nil {
			return err
		}
		return ledger.MigrateLegacyClaims(func(entry localscheduler.ClaimEntry) (localscheduler.ClaimNamespace, error) {
			namespace, resolveErr := legacyClaimNamespace(l, claimProviders, entry)
			if errors.Is(resolveErr, localscheduler.ErrLegacyClaimOwnershipUnresolved) {
				_ = instanceLog.Append(journal.Event{
					Type: journal.EventError, RunID: entry.RunID, Workflow: entry.Workflow,
					Error: &journal.ErrorDetail{
						Code:    "legacy_claim_ownership_unresolved",
						Message: resolveErr.Error(),
					},
				})
			}
			return namespace, resolveErr
		})
	}); err != nil {
		if tel != nil {
			_ = tel.Shutdown(context.Background())
		}
		if rollupDB != nil {
			_ = rollupDB.Close()
		}
		_ = instanceLog.Close()
		return nil, err
	}

	// #712: shared with the Scheduler via SchedulerOptions below — see
	// schedulerSetup.ProviderQuota's doc comment for why a shared pointer,
	// not a Scheduler-owned field, is needed here.
	providerQuota := localscheduler.NewProviderQuotaState()
	runnerRegistry := newDaemonRunnerRegistry()
	definitions, err := buildSchedulerDefinitions(l, cfg, set, report, wg, runnerRegistry, tel, rollupDB, instanceLog, sharedReg, nil, providerQuota, terminalNotifier)
	if err != nil {
		return nil, err
	}
	runnerRegistry.Replace(definitions.Runners)
	legacyRunner, legacyWorktrees, err := buildRetainedLegacyRunner(
		l, cfg, set, tel, instanceLog, sharedReg, providerQuota, terminalNotifier, definitions.HarnessPreflight,
	)
	if err != nil {
		return nil, err
	}
	stableDigest, err := configDirectoryDigest(l.ConfigDir())
	if err != nil || stableDigest != configDigest {
		if tel != nil {
			_ = tel.Shutdown(context.Background())
		}
		if rollupDB != nil {
			_ = rollupDB.Close()
		}
		_ = instanceLog.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("config directory changed during daemon setup; retry startup")
	}

	return &schedulerSetup{
		Runner:            definitions.Runner,
		Runners:           definitions.Runners,
		LegacyRunner:      legacyRunner,
		Telemetry:         tel,
		RollupDB:          rollupDB,
		Config:            cfg,
		Definitions:       definitions.Set,
		Worktrees:         definitions.Worktrees,
		WorktreesByGaggle: definitions.WorktreesByGaggle,
		LegacyWorktrees:   legacyWorktrees,
		InstanceLog:       instanceLog,
		Entries:           definitions.Entries,
		Machines:          definitions.Machines,
		RepoRefs:          definitions.RepoRefs,
		RunConditions:     cfg.RunConditions,
		Validation:        definitions.Validation,
		ConfigDigest:      configDigest,
		RecoveredClaims:   recoveredClaims,
		OpenPRRefresher:   definitions.OpenPRRefresher,
		ProviderQuota:     providerQuota,
		SharedRegistry:    sharedReg,
		TerminalNotifier:  terminalNotifier,
		RunnerRegistry:    runnerRegistry,
	}, nil
}

func legacyClaimNamespace(l instance.Layout, providers map[string]apiv1.Provider, entry localscheduler.ClaimEntry) (localscheduler.ClaimNamespace, error) {
	runDir, err := l.FindRunDir(entry.RunID)
	if err != nil {
		return localscheduler.ClaimNamespace{}, fmt.Errorf("%w: find owning run %q: %w", localscheduler.ErrLegacyClaimOwnershipUnresolved, entry.RunID, err)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return localscheduler.ClaimNamespace{}, fmt.Errorf("%w: open owning run %q: %w", localscheduler.ErrLegacyClaimOwnershipUnresolved, entry.RunID, err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return localscheduler.ClaimNamespace{}, fmt.Errorf("%w: read owning run %q identity: %w", localscheduler.ErrLegacyClaimOwnershipUnresolved, entry.RunID, err)
	}
	if identity.RunID != entry.RunID {
		return localscheduler.ClaimNamespace{}, fmt.Errorf("%w: run journal identity is %q", localscheduler.ErrLegacyClaimOwnershipUnresolved, identity.RunID)
	}
	provider, ok := providers[identity.Gaggle]
	if !ok || provider == "" {
		return localscheduler.ClaimNamespace{}, fmt.Errorf("%w: owning gaggle %q is not configured", localscheduler.ErrLegacyClaimOwnershipUnresolved, identity.Gaggle)
	}
	return localscheduler.ClaimNamespace{
		Gaggle:   identity.Gaggle,
		Provider: string(provider),
	}, nil
}

func claimProvidersByGaggle(set *instance.ConfigSet) map[string]apiv1.Provider {
	providers := make(map[string]apiv1.Provider, len(set.Gaggles))
	for i := range set.Gaggles {
		providers[set.Gaggles[i].Name] = set.Gaggles[i].Spec.Project.Provider
	}
	return providers
}

func buildSchedulerDefinitions(
	l instance.Layout,
	cfg *instance.Config,
	set *instance.ConfigSet,
	report *validate.Report,
	wg *sync.WaitGroup,
	runnerRegistry *daemonRunnerRegistry,
	tel *telemetry.Client,
	rollupDB *rollup.DB,
	instanceLog *journal.InstanceLog,
	sharedReg *journal.RegistryScrubber,
	wtManagers map[string]*worktree.Manager,
	providerQuota *localscheduler.ProviderQuotaState,
	terminalNotifier runner.TerminalNotifier,
) (*schedulerDefinitions, error) {
	goobers := goobersByName(set)
	machines, err := compiledMachines(set, goobers)
	if err != nil {
		return nil, err
	}
	harnessInfo, err := preflightHarnesses(goobers, set.Workflows)
	if err != nil {
		return nil, err
	}
	repoRefs, err := repoRefsByWorkflow(set)
	if err != nil {
		return nil, err
	}

	if wtManagers == nil {
		wtManagers = make(map[string]*worktree.Manager)
	}
	branchNamespaces := branchNamespacesByGaggle(set)
	// Each gaggle's project repo drives its runner's per-gaggle credential
	// scoping (MGV-5, #1012): its stages are granted that repo's own token. A
	// gaggle with no configured Gaggle object (a single-gaggle default) has no
	// entry here, so its runner falls back to the first repo's token unchanged.
	gaggleProjects := make(map[string]apiv1.RepoRef, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleProjects[set.Gaggles[i].Name] = set.Gaggles[i].Spec.Project
	}
	runners := make(map[string]*runner.Runner)
	for _, gaggle := range configuredGaggleNames(set) {
		scoped := l.ForGaggle(gaggle)
		rn, manager, err := buildRuntimeRunner(
			scoped, cfg, goobers, tel, instanceLog, sharedReg, wtManagers[gaggle],
			providerQuota, terminalNotifier, branchNamespaces, gaggleProjects[gaggle], harnessInfo,
		)
		if err != nil {
			return nil, err
		}
		wtManagers[gaggle] = manager
		runners[gaggle] = rn
	}

	openPRRefresher, err := buildOpenPRRefresher(cfg, set.Workflows, sharedReg, branchNamespaces)
	if err != nil {
		return nil, err
	}
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	credResolver, _, err := buildCredentials(cfg, "", "")
	if err != nil {
		return nil, err
	}

	gagglesByName := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for i := range set.Gaggles {
		gagglesByName[set.Gaggles[i].Name] = set.Gaggles[i]
	}

	entries := make([]localscheduler.WorkflowEntry, 0, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		identity := localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}
		machine := machines[identity]
		// #341: a workflow may declare more than one schedule-type trigger
		// (e.g. a weekday cadence and a separate weekend one) — collect all
		// of them rather than stopping at the first; Scheduler.Tick fires if
		// any is due. #342: also collect every signal-type trigger's name —
		// previously compiled nowhere, so a type=signal trigger declared in
		// config did nothing at runtime; Scheduler.Signal fires every
		// workflow subscribed to a received signal name.
		var scheds []localscheduler.Schedule
		var sigs []string
		for _, trigger := range wf.Spec.Triggers {
			if trigger.Type == apiv1.TriggerSchedule && trigger.Schedule != "" {
				schedule, err := localscheduler.ParseSchedule(trigger.Schedule)
				if err != nil {
					return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				scheds = append(scheds, localscheduler.InLocation(schedule, loc))
			}
			if trigger.Type == apiv1.TriggerSignal && trigger.Signal != "" {
				sigs = append(sigs, trigger.Signal)
			}
			if trigger.Type == apiv1.TriggerWebhook {
				for _, event := range trigger.Events {
					sigs = append(sigs, webhookhttp.SignalName(event))
				}
			}
		}
		// RRQ-1/#1101: the runner capabilities a single run of this workflow
		// needs (its gaggle's + its stages'). The scheduler matches them at
		// dispatch against the runner's advertised set (schedule-time), and the
		// runner preflight-verifies the probeable toolchains among them on the
		// host before any stage runs (#735).
		requiredCaps := instance.WorkflowRequiredCapabilities(gagglesByName[wf.Spec.Gaggle], *wf)
		entries = append(entries, localscheduler.WorkflowEntry{
			Workflow:        wf.Name,
			WorkflowVersion: machine.Def.Version,
			WorkflowDigest:  machine.Digest(),
			Gaggle:          wf.Spec.Gaggle,
			Readiness:       wf.Spec.Readiness,
			Schedules:       scheds,
			Signals:         sigs,
			BacklogCounter:  buildBacklogCounter(cfg, wf, repoRefs[identity], credResolver, sharedReg),
			Starter:         &trackedStarter{r: runners[wf.Spec.Gaggle], machine: machine, requiredCaps: requiredCaps, wg: wg, l: l.ForGaggle(wf.Spec.Gaggle), tel: tel, rollupDB: rollupDB, log: instanceLog, runners: runnerRegistry},
			RepoRef:         repoRefs[identity],
			// RRQ-1/#1101 schedule-match + #735 host preflight both consume this.
			RequiredCapabilities: requiredCaps,
		})
	}

	var firstRunner *runner.Runner
	var firstWorktrees *worktree.Manager
	for _, gaggle := range configuredGaggleNames(set) {
		firstRunner = runners[gaggle]
		firstWorktrees = wtManagers[gaggle]
		break
	}
	return &schedulerDefinitions{
		Set:               set,
		Validation:        report,
		HarnessPreflight:  harnessInfo,
		Runner:            firstRunner,
		Runners:           runners,
		Entries:           entries,
		Machines:          machines,
		RepoRefs:          repoRefs,
		OpenPRRefresher:   openPRRefresher,
		Worktrees:         firstWorktrees,
		WorktreesByGaggle: wtManagers,
	}, nil
}

func buildRetainedLegacyRunner(
	l instance.Layout,
	cfg *instance.Config,
	set *instance.ConfigSet,
	tel *telemetry.Client,
	instanceLog *journal.InstanceLog,
	sharedReg *journal.RegistryScrubber,
	providerQuota *localscheduler.ProviderQuotaState,
	terminalNotifier runner.TerminalNotifier,
	harnessInfo harnessPreflightInfo,
) (*runner.Runner, *worktree.Manager, error) {
	retained, err := retainedLegacyRuntimeExists(l)
	if err != nil || !retained {
		return nil, nil, err
	}
	// Legacy retained runtime: no per-gaggle project scoping — a zero project
	// repo leaves credentials on the first-repo default (unchanged behavior).
	return buildRuntimeRunner(
		l, cfg, goobersByName(set), tel, instanceLog, sharedReg, nil, providerQuota,
		terminalNotifier, branchNamespacesByGaggle(set), apiv1.RepoRef{}, harnessInfo,
	)
}

func retainedLegacyRuntimeExists(l instance.Layout) (bool, error) {
	for _, path := range []string{l.RunsDir(), l.WorkcopiesDir()} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect retained legacy runtime %s: %w", path, err)
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return true, nil
		}
	}
	return false, nil
}

func buildRuntimeRunner(
	l instance.Layout,
	cfg *instance.Config,
	goobers map[string]apiv1.GooberSpec,
	tel *telemetry.Client,
	instanceLog *journal.InstanceLog,
	sharedReg *journal.RegistryScrubber,
	manager *worktree.Manager,
	providerQuota *localscheduler.ProviderQuotaState,
	terminalNotifier runner.TerminalNotifier,
	branchNamespaces map[string]string,
	gaggleProject apiv1.RepoRef,
	harnessInfo harnessPreflightInfo,
) (*runner.Runner, *worktree.Manager, error) {
	runnerCfg, manager, err := buildRunnerConfig(
		l, cfg, goobers, tel, sharedReg, manager, branchNamespaces, gaggleProject, harnessInfo,
	)
	if err != nil {
		return nil, nil, err
	}
	runnerCfg.PrepareTerminal, err = buildTerminalBranchPreparer(l, cfg, sharedReg)
	if err != nil {
		return nil, nil, err
	}
	runnerCfg.FinalizeTerminal = func(runID string, _ journal.RunPhase) error {
		return finalizeTerminalRun(l, instanceLog, manager, runID)
	}
	runnerCfg.RateLimited = buildRateLimitedHandler(providerQuota)
	runnerCfg.NotifyTerminal = terminalNotifier
	rn, err := runner.New(runnerCfg)
	if err != nil {
		return nil, nil, err
	}
	return rn, manager, nil
}

func configuredGaggleNames(set *instance.ConfigSet) []string {
	names := make([]string, 0, len(set.Gaggles))
	for i := range set.Gaggles {
		names = append(names, set.Gaggles[i].Name)
	}
	sort.Strings(names)
	return names
}

// SchedulerOptions returns the localscheduler.Option slice reflecting this
// setup's telemetry state — no telemetry options when it is disabled (issue
// #129).
// See buildSchedulerSetup's doc comment for why a nil Telemetry must never
// reach localscheduler.WithTelemetry directly.
func (s *schedulerSetup) SchedulerOptions() []localscheduler.Option {
	// ProviderQuota (#712) needs no background loop (event-driven, not
	// polled — see its own doc comment), so unlike OpenPRRefresher it's wired
	// here uniformly for every caller (both `up` and `run`), not gated behind
	// an up.go-only branch.
	opts := []localscheduler.Option{localscheduler.WithProviderQuota(s.ProviderQuota)}
	// RRQ-1/#1101: the local runner's static advertised capability set, so
	// dispatch can refuse a run whose gaggle/stages require a capability this
	// runner does not claim. Wired uniformly for both `up` and `run`.
	if s.Config != nil {
		opts = append(opts, localscheduler.WithRunnerCapabilities(s.Config.Runner.Capabilities))
	}
	if s.Telemetry != nil {
		opts = append(opts, localscheduler.WithTelemetry(s.Telemetry))
		if s.RollupDB != nil && s.InstanceLog != nil {
			opts = append(opts, localscheduler.WithAfterTick(func(ctx context.Context) {
				ingestSchedulerTelemetry(ctx, s.Telemetry, s.RollupDB, s.InstanceLog.Dir(), s.InstanceLog)
			}))
		}
	}
	if s.Telemetry != nil && s.RollupDB != nil {
		opts = append(opts, localscheduler.WithAfterTick(func(ctx context.Context) {
			if err := s.Telemetry.Flush(ctx); err != nil {
				logIngestFailure(s.InstanceLog, "", "telemetry_flush_scheduler_failed", err)
			}
			s.ingestSchedulerLog()
		}))
	}
	return opts
}

func (s *schedulerSetup) ingestSchedulerLog() {
	if s.RollupDB == nil || s.InstanceLog == nil {
		return
	}
	if err := s.RollupDB.IngestSchedulerLog(s.InstanceLog.Dir()); err != nil {
		logIngestFailure(s.InstanceLog, "", "telemetry_ingest_scheduler_log_failed", err)
	}
}

// Shutdown flushes/closes the telemetry client, ingests any final scheduler
// spans, and closes the rollup db. It is nil-safe so a caller can defer it
// unconditionally regardless of whether instance.yaml enabled telemetry
// (issue #129).
func (s *schedulerSetup) Shutdown(ctx context.Context) {
	if s.Telemetry != nil {
		_ = s.Telemetry.Shutdown(ctx)
	}
	if s.RollupDB != nil {
		s.ingestSchedulerLog()
		_ = s.RollupDB.Close()
	}
}

// trackedStarter adapts a *runner.Runner + its compiled Machine into a
// localscheduler.Starter — one per workflow, per that seam's doc comment
// ("#17's *runner.Runner is bound to a single compiled machine at
// construction, so the scheduler holds a map of workflow name -> Starter").
// It also tracks every dispatched run in wg so the daemon's shutdown drain
// (runUpContext) waits for scheduler-dispatched runs, not just the startup
// resume scan's — wg.Add happens inside Start, which localscheduler's own
// dispatch already calls from its own goroutine, so there is an inherent
// (and accepted) small race window between that goroutine launching and
// wg.Add actually running; closing it fully would need a scheduler-side
// hook this seam doesn't expose. Every dispatch through this Starter — both
// `goobers up`'s scheduled/manual-via-Trigger fires and `goobers run`'s own
// sched.Trigger call, now that #134 routes it through the same scheduler —
// incrementally ingests into rollupDB on completion (issue #127).
type trackedStarter struct {
	r            *runner.Runner
	machine      *workflow.Machine
	requiredCaps []string
	wg           *sync.WaitGroup
	l            instance.Layout
	tel          *telemetry.Client
	rollupDB     *rollup.DB
	log          *journal.InstanceLog
	runners      *daemonRunnerRegistry
}

func (s *trackedStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.wg.Add(1)
	defer s.wg.Done()
	untrack := s.runners.Track(req.RunID, s.r)
	defer untrack()
	res, err := s.r.Start(ctx, runner.StartInput{
		RunID:                req.RunID,
		Machine:              s.machine,
		Gaggle:               req.Gaggle,
		Trigger:              req.Trigger,
		RepoRef:              req.RepoRef,
		Item:                 req.Item,
		RequiredCapabilities: s.requiredCaps,
	})
	ingestRunTelemetry(s.tel, s.rollupDB, s.l, req.RunID, s.log)
	return localscheduler.StartResult{
		Phase:          res.Phase,
		FinalState:     res.FinalState,
		FailureStage:   res.FailureStage,
		FailureCode:    res.FailureCode,
		FailureMessage: res.FailureMessage,
	}, err
}

// resumeInterruptedRuns scans runsDir for any run left non-terminal by a
// prior crash or unclean daemon shutdown and restarts it via Runner.Resume,
// each in its own goroutine tracked by wg — the daemon-startup recovery pass
// (issue #23 AC: restart via Runner.Resume). "Interrupted" is exactly
// journal.PhaseRunning in the event log: no run.finished event has landed.
// Resume itself is idempotent on an already-terminal run and safe to call on
// one that merely paused gracefully (a human gate, or a prior clean drain),
// not only a genuine crash — so this scan doesn't need to distinguish those
// cases itself; a gate-paused run's Resume call returns almost immediately
// (walk re-checkpoints at the same gate without evaluating anything), so its
// reserved slot (below) is held only briefly, not for the daemon's lifetime.
// Runs already terminal in their event log are not resumed, but their claims
// and any reconciled concurrency slot are released idempotently to keep the
// reconciliation and resume passes in agreement.
//
// release is called with each recovered run and workflow — immediately for a
// terminal run, or once a resumed run's Resume call returns (success or
// error). Scheduler.ReleaseReconciled only releases runs actually seeded by
// Reconcile, so terminal cleanup cannot consume another run's slot.
//
// A run whose workflow or gaggle no longer resolves in the current config
// (renamed or removed, issue #135 point 2) is skipped with a warning journaled
// to log, not a fatal error — a stale run must never prevent the daemon from
// starting; recovering it is `goobers run abort <run-id>` (abort.go).
//
// Each resumed run also incrementally ingests into rollupDB once its outcome
// is known (issue #127), the same hook trackedStarter.Start uses for a live
// dispatch — a resumed run's spans/errors/stage_attempts must show up in
// `goobers telemetry` too, not just a freshly-dispatched one's. tel is
// flushed first (issue #129), same ordering rationale as
// trackedStarter.Start — the batched span exporter must write spans.jsonl to
// disk before ingest reads it.
//
// resumeInterruptedRuns errors when the scan itself cannot proceed or when
// terminal-run cleanup fails; claim cleanup fails closed rather than silently
// leaving a known terminal owner in the ledger.
func resumeInterruptedRuns(ctx context.Context, l instance.Layout, rn *runner.Runner, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, release func(runID, workflow string), wg *sync.WaitGroup) (resumed []string, warned []string, err error) {
	return resumeInterruptedRunsWithRunners(ctx, l, nil, rn, nil, machines, repoRefs, log, tel, rollupDB, release, wg)
}

func resumeInterruptedRunsWithRunners(ctx context.Context, l instance.Layout, runners map[string]*runner.Runner, fallback *runner.Runner, runnerRegistry *daemonRunnerRegistry, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, release func(runID, workflow string), wg *sync.WaitGroup) (resumed []string, warned []string, err error) {
	runDirs, err := l.RunDirs()
	if err != nil {
		return nil, nil, err
	}
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return resumed, warned, fmt.Errorf("read runs directory: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(runsDir, e.Name())
			rd, err := journal.OpenRead(dir)
			if err != nil {
				continue // not a run directory
			}
			id, err := rd.Identity()
			if err != nil {
				continue
			}
			rn := fallback
			runLayout := l
			if filepath.Clean(runsDir) != filepath.Clean(l.RunsDir()) {
				runLayout = l.ForGaggle(id.Gaggle)
			}
			if runners != nil && runLayout.Gaggle() != "" {
				rn = runners[id.Gaggle]
			}
			// Event-log-first (#242): state.json can lag a crash-fsynced
			// run.finished event, so Phase() (reconstructed from the log) is
			// what decides whether this run is actually terminal — trusting
			// the checkpoint directly here risks spinning up a resume
			// goroutine for a run that already finished.
			if phase, err := rd.Phase(); err == nil {
				switch phase {
				case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
					var finalizeErr error
					if rn != nil {
						finalizeErr = rn.FinalizeTerminal(id.RunID, phase)
					} else {
						manager, managerErr := worktree.NewManager(runLayout.WorkcopiesDir())
						if managerErr != nil {
							finalizeErr = managerErr
						} else {
							finalizeErr = finalizeTerminalRun(runLayout, log, manager, id.RunID)
						}
					}
					if finalizeErr != nil {
						return resumed, warned, fmt.Errorf("finalize terminal run %q: %w", id.RunID, finalizeErr)
					}
					release(id.RunID, id.Workflow)
					continue // terminal: nothing to resume
				}
			}

			identity := localscheduler.WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}
			machine, ok := machines[identity]
			if rn == nil || !ok {
				warned = append(warned, id.RunID)
				if log != nil {
					code := "resume_unresolvable_workflow"
					message := fmt.Sprintf("run %q references unknown workflow %q — recover with `goobers run abort %s`", id.RunID, id.Workflow, id.RunID)
					if rn == nil {
						code = "resume_unresolvable_gaggle"
						message = fmt.Sprintf("run %q references inactive gaggle %q — recover with `goobers run abort %s`", id.RunID, id.Gaggle, id.RunID)
					}
					_ = log.Append(journal.Event{
						Type: journal.EventError, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
						Error: &journal.ErrorDetail{
							Code:    code,
							Message: message,
						},
					})
				}
				continue
			}
			repoRef := repoRefs[identity]

			resumed = append(resumed, id.RunID)
			wg.Add(1)
			untrack := runnerRegistry.Track(id.RunID, rn)
			go func(runID, gaggle, wfName string, rn *runner.Runner, runLayout instance.Layout, untrack func()) {
				defer wg.Done()
				defer release(runID, wfName)
				defer untrack()
				result, err := rn.Resume(ctx, runner.ResumeInput{RunID: runID, Machine: machine, RepoRef: repoRef})
				ingestRunTelemetry(tel, rollupDB, runLayout, runID, log)
				// #710: same fix as localscheduler/scheduler.go's dispatch echo —
				// a business failure (result.Phase == PhaseFailed, err == nil:
				// e.g. a WF-016 refuseResume, or Resume replaying a stage's own
				// business-failure terminal transition) used to echo a bare
				// "failed" here too. result is runner.Result directly (this path
				// calls Runner.Resume, not through the scheduler's Starter seam),
				// so FailureStage/Code/Message need no extra mirroring. The
				// infra-error branch is deliberately untouched: a genuine Go
				// error from Resume already carries its own full detail.
				ev := journal.Event{Type: journal.EventRunFinished, Gaggle: gaggle, Workflow: wfName, RunID: runID, Status: string(result.Phase)}
				switch {
				case err != nil:
					ev.Status = "error: " + err.Error()
				case result.FailureCode != "":
					ev.Stage = result.FailureStage
					ev.Error = &journal.ErrorDetail{Code: result.FailureCode, Message: result.FailureMessage}
					if result.FailureStage != "" {
						ev.Status = fmt.Sprintf("%s (%s: %s)", ev.Status, result.FailureStage, result.FailureCode)
					} else {
						ev.Status = fmt.Sprintf("%s (%s)", ev.Status, result.FailureCode)
					}
				}
				if log != nil {
					_ = log.Append(ev)
				}
			}(id.RunID, id.Gaggle, id.Workflow, rn, runLayout, untrack)
		}
	}
	return resumed, warned, nil
}

// waitDrained waits for wg to finish, returning false if timeout elapses
// first. The background goroutine it starts is not leaked: wg.Wait()
// returning always lets it close done and exit, whether or not the select
// below already gave up waiting.
func waitDrained(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
