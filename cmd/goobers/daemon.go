package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/sessionops"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/telemetry"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/workcopyroot"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

const legacyRuntimeMigrationNote = "legacy flat runtime migrated to per-gaggle layout"

// schedulerSetup exposes the daemon's assembled dependencies to up and run.
// Observation and runtime own resource cleanup; the remaining fields are views
// used by scheduler, reload and API wiring. Shutdown drains them in order.
type schedulerSetup struct {
	SessionBacklogReader   sessionops.ReaderFactory
	SessionBacklogWriter   sessionops.WriterFactory
	SessionBacklogResolver sessionops.ResolverFactory
	SessionGeneration      string
	SessionRuntime         *daemonSessionRuntime
	OrdinaryRuntime        ordinaryRuntimeBuilder
	OrdinaryCatalog        *ordinaryStartCatalog
	SourceStarts           localscheduler.SourceQueue
	ChildRuntime           childRuntimeBuilder
	EventRuntime           eventRuntimeBuilder
	EventCatalog           eventPublicationSnapshot
	EventPublisher         *daemonEventPublisher
	observation            *schedulerObservation
	runtime                *schedulerRuntime
	Generations            *configgeneration.Retainer
	Root                   string
	Runner                 *runner.Runner
	Runners                map[string]*runner.Runner
	LegacyRunner           *runner.Runner
	Telemetry              *telemetry.Client
	// Shared only by this setup's trace, journal, and diagnostic exporters.
	TelemetryReplayStart <-chan struct{}
	RollupDB             *rollup.DB
	// ReadModel is the portal run read model (read.db). Present but unread at
	// this stage — see the construction site and design §6.6 step 1.
	ReadModel *readmodel.Store
	// Watermarks is the source-watermark store (#1922). Separate from ReadModel
	// because they are different databases with different writers: anything that
	// advances a run records here, while only the projector touches ReadModel.
	Watermarks *intake.Store
	// ProjectorRestartComplete means the bounded pending/non-terminal startup
	// catch-up succeeded, so read-model recovery inventory is safe to consume.
	ProjectorRestartComplete bool
	// StopProjector shuts the projector's commit loop down. Held on the setup so
	// the daemon's shutdown path stops it with everything else, rather than the
	// loop outliving the process's other goroutines.
	StopProjector func()
	// RetentionStats snapshots projection-retention loop counters for status
	// diagnostics while the daemon is running.
	RetentionStats func() readmodel.RetentionStats
	// ProjectorStats snapshots the projector's counters, including the runs it
	// failed to apply and when it last completed a pass. The read service turns
	// them into the readState envelope's gap and lag signals (#2843).
	ProjectorStats func() projector.Stats
	// ReadModelEpoch is the store's opaque per-build identity (§4.2), read back
	// at open so a broken store surfaces at daemon start rather than on the first
	// read. It becomes the SSE cursor's epoch component in Wave 5.
	ReadModelEpoch    string
	Config            *instance.Config
	Definitions       *instance.ConfigSet
	Worktrees         *worktree.Manager
	WorktreesByGaggle map[string]*worktree.Manager
	LegacyWorktrees   *worktree.Manager
	InstanceLog       *journal.InstanceLog
	Entries           []localscheduler.WorkflowEntry
	Machines          map[localscheduler.WorkflowIdentity]*workflow.Machine
	GooberDigests     map[localscheduler.WorkflowIdentity]string
	RepoRefs          map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	RunConditions     instance.RunConditions
	Validation        *validate.Report
	ConfigDigest      string
	RecoveredClaims   []localscheduler.ClaimEntry
	// OpenPRRefresher backs the #353 MaxOpenPRs cap — one refresher per
	// distinct gaggle repo (#2692); nil when no workflow opts in (or no repo
	// is configured). Only the `up` daemon starts its Run loop and wires it as
	// a scheduler option — see up.go.
	OpenPRRefresher *localscheduler.OpenPRRefresherSet
	// EngineRuntime is the late-bound engine wiring every engineStarter these
	// entries carry shares. up.go attaches it once the Temporal client and
	// live journal writer exist; see engineRuntime for why that cannot happen
	// at definition-build time.
	EngineRuntime *engineRuntime
	// ProviderQuota is the shared provider budget ledger. Stage rate-limit
	// failures and provider response headers write to it; SchedulerOptions wires
	// the same pointer into polling and run admission. Unlike OpenPRRefresher it
	// needs no background Run loop, so it is wired uniformly for `up` and `run`.
	// Never nil.
	ProviderQuota    *localscheduler.ProviderQuotaState
	SharedRegistry   *journal.RegistryScrubber
	TerminalNotifier runner.TerminalNotifier
	RunnerRegistry   *daemonRunnerRegistry
	// Interventions is the atomically replaced definition snapshot used by the
	// daemon's mutation service during config reload.
	Interventions *interventionDefinitionRegistry
	// CredentialPlane is the daemon credential service (#3511); set by up.go
	// after API wiring so config reload can swap its config-derived snapshot
	// alongside the intervention definitions. Nil outside the `up` daemon.
	CredentialPlane   *daemonCredentialService
	InteractiveAccess *interactiveaccess.Service
	ChildRestarts     *queuedChildLauncher
	// Installed only when the dedicated human execution builder is available.
	InteractiveRestartExecution func(context.Context, runner.StageRestartPlan) (intervention.Execution, error)
	InteractiveRestartRecovery  func(context.Context, journal.RunIdentity) (intervention.Execution, error)
	// SecretStores resolves store-backed token refs (#683). Built once per
	// setup from cfg.SecretStores so every consumer shares one TTL cache;
	// never nil — an instance with no declared stores gets a registry that
	// fails every store ref closed.
	SecretStores *secretstore.Registry
	// TelemetryExporterHealth exposes the daemon-local exporter health monitor
	// to read surfaces. It is non-nil even when telemetry is disabled so those
	// surfaces can report an explicit disabled state.
	TelemetryExporterHealth *telemetry.ExporterHealth
	// MergedPRCostReconciler is the daemon-owned, workflow-independent
	// backstop that publishes cost summaries for recently merged Goobers PRs.
	// Config reload replaces its definition snapshot in place.
	MergedPRCostReconciler *daemonMergedPRCostReconciler

	// shutdownOnce/shutdownErr make Shutdown idempotent: `up` closes the setup
	// explicitly so a flush or close failure can fail the command, while the
	// early-return defer stays in place as a safety net. Whichever runs first
	// owns the close; the other observes the same result — including a
	// memoized deadline error, which a later caller sees even if the step that
	// blew the grace period finished afterwards.
	shutdownOnce sync.Once
	shutdownErr  error
}

func newTelemetryExporterHealth(cfg *instance.Config) *telemetry.ExporterHealth {
	if cfg == nil || !cfg.TelemetryEnabled() {
		return telemetry.NewExporterHealth(false, "disabled", "")
	}
	if len(cfg.Telemetry.Exporters) > 0 {
		return telemetry.NewExporterHealth(true, "custom", "")
	}
	mode := "local"
	endpoint := ""
	otlpEnabled := cfg.Telemetry.OTLP != nil && cfg.Telemetry.OTLP.Enabled()
	azureEnabled := cfg.Telemetry.AzureMonitor.Enabled()
	if otlpEnabled && azureEnabled {
		mode = "custom"
		endpoint = cfg.Telemetry.OTLP.Endpoint
	} else if otlpEnabled {
		mode = string(telemetry.ExporterOTLP)
		endpoint = cfg.Telemetry.OTLP.Endpoint
	} else if azureEnabled {
		mode = "azure-monitor"
	}
	return telemetry.NewExporterHealth(true, mode, endpoint)
}

func logTelemetryOTLPUnavailable(log *journal.InstanceLog, cause error) {
	if log == nil {
		return
	}
	log.AppendBestEffort(journal.Event{
		Type: journal.EventError,
		Error: &journal.ErrorDetail{
			Code:    "telemetry_otlp_unavailable",
			Message: telemetry.ExporterFailureReason(cause),
		},
	})
}

type schedulerDefinitions struct {
	OrdinaryRuntime    ordinaryRuntimeBuilder
	EventCatalog       eventPublicationSnapshot
	ChildRuntime       childRuntimeBuilder
	EventRuntime       eventRuntimeBuilder
	GenerationResolver executionGenerationResolver
	Set                *instance.ConfigSet
	Validation         *validate.Report
	HarnessPreflight   harnessPreflightInfo
	Runner             *runner.Runner
	Runners            map[string]*runner.Runner
	Entries            []localscheduler.WorkflowEntry
	Machines           map[localscheduler.WorkflowIdentity]*workflow.Machine
	GooberDigests      map[localscheduler.WorkflowIdentity]string
	Goobers            map[string]apiv1.GooberSpec
	RepoRefs           map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	OpenPRRefresher    *localscheduler.OpenPRRefresherSet
	// EngineRuntime is the late-bound holder every engineStarter these
	// definitions installed shares; up.go attaches it once the Temporal
	// client and live journal writer exist. See engineRuntime.
	EngineRuntime     *engineRuntime
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
	reportStartupProgress(options.startupProgress, "loading instance and workflow configuration")
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return nil, err
	}
	// One store registry per setup (#683): every store-backed token ref below
	// — repo tokens, per-capability credentials, webhook secret, OTLP headers
	// — resolves through this single TTL-cached registry.
	secretStores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, err
	}
	// #3314: a first boot whose config comes from a remote workflowSource has
	// no config directory yet — the daemon is what fetches it — so the tree is
	// seeded before it is digested.
	configDigest, err := bootstrapAndDigestConfigDir(l, cfg)
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
	reportStartupProgress(options.startupProgress, fmt.Sprintf(
		"loaded configuration (%d gaggle(s), %d workflow(s))",
		len(set.Gaggles), len(set.Workflows),
	))
	defer func() {
		if err != nil {
			err = &configReportError{report: report, err: err}
		}
	}()
	// MGV-1/#1009: resolve each gaggle's declared CI command into its local-ci
	// stage before the workflows are compiled, so the runner executes the
	// gaggle's own suite in place of the stage's declared `make ci` default.
	instance.ApplyGaggleCICommand(set)
	// Missing runner capabilities can heal as workers join, so warn here and
	// let per-run admission refuse affected workflows. Declared inventories
	// instead use the per-workflow placement solve during definition building.
	if len(cfg.Runners) == 0 {
		if err := instance.CheckCapabilityRequirements(cfg.SelfRunnerCapabilities(), set); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v; affected runs are refused at schedule time with the capability named, other gaggles are unaffected\n", err)
		}
	}
	// CONF-6/#2079: fail closed at startup when a workflow requires a provider
	// capability its gaggle's connected provider does not declare — a
	// provider's declared capabilities can't change without a code deploy, so
	// unlike a missing runner capability this can never self-heal at runtime;
	// catch it here rather than at the first ErrUnsupported mid-run.
	if err := instance.CheckProviderCapabilityRequirements(set); err != nil {
		return nil, err
	}
	gaggles := configuredGaggleNames(set)
	runtimeMigration, err := l.MigrateLegacyRuntimeWithReport(gaggles)
	if err != nil {
		return nil, err
	}
	// Goobers#3989: fold the pre-keyed aggregate pr-remediation-noop.json into
	// the per-PR scheduler-state keys before anything can read one. The record
	// is a loop breaker, so losing it across the upgrade would spend a full
	// agentic remediation cycle per currently-suppressed PR proving again that
	// there is nothing to do.
	if err := migrateLegacyRemediationNoopState(l); err != nil {
		return nil, err
	}
	// This daemon owns identity creation (workers and telemetry observers do
	// not). Publish it before any exporter or scheduler journal captures its
	// identity: creating it later in runner construction left first-boot
	// scheduler records unidentified until restart and changed their replay
	// identity inputs across those lifetimes.
	if _, err := l.EnsureIdentity(ctx); err != nil {
		return nil, fmt.Errorf("initialize daemon instance identity: %w", err)
	}

	// Share credential redaction across instance-lifetime telemetry and journals.
	sharedReg := journal.NewRegistryScrubber()
	sharedScrubber := journal.Chain(sharedReg, journal.NewPatternScrubber())
	terminalNotifier, err := buildTerminalNotifier(ctx, l, cfg, sharedScrubber, options)
	if err != nil {
		return nil, err
	}
	observation, err := openSchedulerObservation(ctx, l, cfg, sharedScrubber, sharedReg, secretStores, options)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = observation.Close()
		}
	}()
	if err := journalLegacyRuntimeMigration(l, observation.instanceLog, runtimeMigration); err != nil {
		return nil, fmt.Errorf("journal legacy runtime migration: %w", err)
	}
	reportStartupProgress(options.startupProgress, "recovering scheduler claims")
	claimState, err := openSchedulerClaimState(schedulerClaimInput{Layout: l, RecoveryGate: options.claimRecoveryGate, InstanceLog: observation.instanceLog, Providers: claimProvidersByGaggle(set)})
	if err != nil {
		return nil, err
	}
	reportStartupProgress(options.startupProgress, "scheduler claims recovered")

	runtime, err := openSchedulerRuntime(schedulerDefinitionsInput{
		Layout: l, Config: cfg, Definitions: set, Validation: report,
		WaitGroup: wg, Telemetry: observation.tel, RollupDB: observation.rollupDB,
		Watermarks: observation.watermarks, InstanceLog: observation.instanceLog,
		SharedRegistry: sharedReg, TerminalNotifier: terminalNotifier,
		CredentialStores: secretStores, StartupProgress: options.startupProgress,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = runtime.Close()
		}
	}()
	definitions := runtime.definitions
	stableDigest, err := configDirectoryDigest(l.ConfigDir())
	if err != nil || stableDigest != configDigest {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("config directory changed during daemon setup; retry startup")
	}

	return &schedulerSetup{
		observation:              observation,
		runtime:                  runtime,
		SessionGeneration:        definitions.sessionGeneration(),
		ChildRuntime:             definitions.ChildRuntime,
		EventRuntime:             definitions.EventRuntime,
		OrdinaryRuntime:          definitions.OrdinaryRuntime,
		EventCatalog:             definitions.EventCatalog,
		Generations:              runtime.generations,
		Root:                     l.Root,
		Runner:                   definitions.Runner,
		Runners:                  definitions.Runners,
		LegacyRunner:             runtime.legacyRunner,
		Telemetry:                observation.tel,
		TelemetryReplayStart:     options.telemetryReplayStart,
		RollupDB:                 observation.rollupDB,
		ReadModel:                observation.readModel,
		Watermarks:               observation.watermarks,
		ProjectorRestartComplete: observation.projectorRestartComplete,
		StopProjector:            observation.stopProjector,
		RetentionStats:           observation.retentionStats,
		ProjectorStats:           observation.projectorStats,
		ReadModelEpoch:           observation.readModelEpoch,
		Config:                   cfg,
		Definitions:              definitions.Set,
		Worktrees:                definitions.Worktrees,
		WorktreesByGaggle:        definitions.WorktreesByGaggle,
		LegacyWorktrees:          runtime.legacyWorktrees,
		InstanceLog:              observation.instanceLog,
		Entries:                  definitions.Entries,
		Machines:                 definitions.Machines,
		GooberDigests:            definitions.GooberDigests,
		RepoRefs:                 definitions.RepoRefs,
		RunConditions:            cfg.RunConditions,
		Validation:               definitions.Validation,
		ConfigDigest:             configDigest,
		RecoveredClaims:          claimState.recovered,
		OpenPRRefresher:          definitions.OpenPRRefresher,
		EngineRuntime:            definitions.EngineRuntime,
		ProviderQuota:            runtime.providerQuota,
		SharedRegistry:           sharedReg,
		TerminalNotifier:         terminalNotifier,
		RunnerRegistry:           runtime.runnerRegistry,
		Interventions:            runtime.interventions,
		SecretStores:             secretStores,
		TelemetryExporterHealth:  observation.telemetryExporterHealth,
	}, nil
}

func reportStartupProgress(report func(string), message string) {
	if report != nil {
		report(message)
	}
}

func journalLegacyRuntimeMigration(l instance.Layout, instanceLog *journal.InstanceLog, migration instance.RuntimeMigration) error {
	if len(migration.MovedDirs) == 0 {
		return nil
	}
	events, err := journal.ReadInstanceLog(instanceLog.Dir())
	if err != nil {
		return fmt.Errorf("read instance log: %w", err)
	}
	journaled := false
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["note"] == legacyRuntimeMigrationNote &&
			event.Runner["migrationId"] == migration.ID {
			journaled = true
			break
		}
	}
	if !journaled {
		if err := instanceLog.Append(legacyRuntimeMigrationEvent(migration)); err != nil {
			return err
		}
	}
	return l.CompleteLegacyRuntimeMigration(migration)
}

func legacyRuntimeMigrationEvent(migration instance.RuntimeMigration) journal.Event {
	return journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"note":             legacyRuntimeMigrationNote,
			"migrationId":      migration.ID,
			"gaggle":           migration.Gaggle,
			"movedDirectories": migration.MovedDirs,
		},
	}
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

type workcopyRootClaim struct {
	gaggle    string
	alternate bool
}

func claimWorkcopyRoot(claims map[string]workcopyRootClaim, gaggle, root string, alternate bool) error {
	path, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve workcopies path for gaggle %s: %w", gaggle, err)
	}
	key, err := workcopyroot.Key(path)
	if err != nil {
		return fmt.Errorf("resolve workcopies path for gaggle %s: %w", gaggle, err)
	}
	if other, exists := claims[key]; exists && other.gaggle != gaggle && (alternate || other.alternate) {
		return fmt.Errorf("workcopies path collision: gaggles %s and %s resolve to %s", other.gaggle, gaggle, path)
	}
	claims[key] = workcopyRootClaim{gaggle: gaggle, alternate: alternate}
	return nil
}

// triggerDisabled reports whether Trigger.Enabled explicitly suppresses this
// trigger. The layered contract is: Enabled=nil preserves historical enabled
// behavior; Enabled=false suppresses the trigger for every non-manual type
// (schedule, signal, webhook, backlog-item) so the daemon does not register
// a schedule, wait for a signal, subscribe to webhook events, or use its
// polling priority. Manual triggers exist to declare a workflow that never
// auto-fires and always ignore Enabled.
func triggerDisabled(trigger apiv1.Trigger) bool {
	if trigger.Enabled == nil || *trigger.Enabled {
		return false
	}
	return trigger.Type != apiv1.TriggerManual
}

// webhookTriggerSignalsAndBackoff derives a type=webhook trigger's signal
// names and idle-backoff policy (#4262), factored out of
// buildSchedulerDefinitions's per-trigger loop to keep that function's
// complexity within the gate.
func webhookTriggerSignalsAndBackoff(workflowName string, trigger apiv1.Trigger) ([]string, localscheduler.IdleBackoffConfig, error) {
	sigs := make([]string, 0, len(trigger.Events))
	for _, event := range trigger.Events {
		sigs = append(sigs, webhookhttp.SignalName(event))
	}
	backoff, err := localscheduler.ParseIdleBackoff(trigger.IdleBackoff)
	if err != nil {
		return nil, localscheduler.IdleBackoffConfig{}, fmt.Errorf("workflow %q: %w", workflowName, err)
	}
	return sigs, backoff, nil
}

func buildSchedulerDefinitions(input schedulerDefinitionsInput) (*schedulerDefinitions, error) {
	l := input.Layout

	l, generation, err := retainOptionalExecutionGeneration(l, input.Generations)
	if err != nil {
		return nil, err
	}
	// Resolve gaggle CI commands on every compilation path.
	instance.ApplyGaggleCICommand(input.Definitions)
	instance.ApplyGaggleOutboxMirror(input.Definitions)
	goobers := goobersByName(input.Definitions)
	if err := validateStoredCopilotAuthBoundaries(input.Config, input.Definitions, goobers); err != nil {
		return nil, err
	}
	instructions, err := loadGooberInstructions(l.ConfigDir(), goobers)
	if err != nil {
		return nil, err
	}
	// Admission and the sign-in preflight share this credential source (#4292).
	modelCredential, _, err := agentModelCredentialResolver(input.Config, input.CredentialStores, "")
	if err != nil {
		return nil, err
	}
	machines, gooberDigests, resolvedGoobers, harnessWarnings, err := compileSchedulerMachinesWithProgress(
		l, input.Config, input.Definitions, goobers, instructions, modelCredential, input.StartupProgress,
	)
	if err != nil {
		return nil, err
	}
	if _, err := appendGooberHarnessWarnings(input.Validation, harnessWarnings); err != nil {
		return nil, fmt.Errorf("append harness validation warnings: %w", err)
	}
	harnessInfo, harnessRefusals, err := preflightSchedulerHarnessesWithProgress(input.Config, input.Definitions, goobers, input.CredentialStores, input.StartupProgress)
	if err != nil {
		return nil, err
	}
	repoRefs, err := repoRefsByWorkflow(input.Definitions)
	if err != nil {
		return nil, err
	}

	input.WorktreeManagers = clonedWorktreeManagers(input.WorktreeManagers)
	branchNamespaces := branchNamespacesByGaggle(input.Definitions)
	selfIdentities := selfIdentitiesByGaggle(input.Config, input.Definitions)
	requireLabelsDefaults := requireLabelsByGaggle(input.Definitions)
	backlogLabelsDefaults := backlogLabelsByGaggle(input.Definitions)
	backlogLabelPredicateDefaults, ownershipDefaults := backlogLabelPredicatesByGaggle(input.Definitions), issueOwnershipDefaultsByGaggle(input.Definitions)
	// Each gaggle's project repo drives its runner's per-gaggle credential
	// scoping (MGV-5, #1012): its stages are granted that repo's own token. A
	// gaggle with no configured Gaggle object (a single-gaggle default) has no
	// entry here, so its runner falls back to the first repo's token unchanged.
	gaggleProjects := make(map[string]apiv1.RepoRef, len(input.Definitions.Gaggles))
	gaggleAdditionalRepos := make(map[string][]apiv1.RepoRef, len(input.Definitions.Gaggles))
	workcopyLayouts := make(map[string]instance.Layout, len(input.Definitions.Gaggles))
	workcopyRoots := make(map[string]workcopyRootClaim, len(input.Definitions.Gaggles))
	for i := range input.Definitions.Gaggles {
		gaggle := &input.Definitions.Gaggles[i]
		gaggleProjects[gaggle.Name] = gaggle.Spec.Project
		gaggleAdditionalRepos[gaggle.Name] = gaggle.Spec.AdditionalRepos
		scoped, layoutErr := instance.EffectiveWorkcopiesLayout(l.ForGaggle(gaggle.Name), input.Config, gaggle)
		if layoutErr != nil {
			return nil, fmt.Errorf("gaggle %s: %w", gaggle.Name, layoutErr)
		}
		managerRoot := scoped.WorkcopiesDir()
		if configuredProject, ok := configuredRepoForProject(input.Config, gaggle.Spec.Project); ok && configuredProject.Pinned() {
			managerRoot = scoped.WorkcopiesBaseDir()
		}
		alternateRoot := input.Config.Workcopies != nil && input.Config.Workcopies.Root != ""
		if gaggle.Spec.Workcopies != nil && gaggle.Spec.Workcopies.Root != "" {
			alternateRoot = true
		}
		if err := claimWorkcopyRoot(workcopyRoots, gaggle.Name, managerRoot, alternateRoot); err != nil {
			return nil, err
		}
		workcopyLayouts[gaggle.Name] = scoped
	}
	sandboxPostures := sandboxPosturesByGaggle(input.Config, input.Definitions)
	runners := make(map[string]*runner.Runner)
	engineHooks := make(map[string]*engineTerminalHooks)
	for _, gaggle := range configuredGaggleNames(input.Definitions) {
		reportStartupProgress(input.StartupProgress, fmt.Sprintf("initializing gaggle %q runtime", gaggle))
		scoped := workcopyLayouts[gaggle]
		rn, manager, hooks, err := buildRuntimeRunner(runtimeRunnerInput{
			Definitions:                  input.Definitions,
			Layout:                       scoped,
			Config:                       input.Config,
			Goobers:                      resolvedGoobers,
			InstructionsByGoober:         instructions,
			Telemetry:                    input.Telemetry,
			InstanceLog:                  input.InstanceLog,
			SharedRegistry:               input.SharedRegistry,
			WorktreeManager:              input.WorktreeManagers[gaggle],
			ProviderQuota:                input.ProviderQuota,
			Watermarks:                   input.Watermarks,
			TerminalNotifier:             input.TerminalNotifier,
			BranchNamespaces:             branchNamespaces,
			GaggleProject:                gaggleProjects[gaggle],
			GaggleBacklog:                gaggleBacklogRef(input.Definitions, gaggle),
			AdditionalRepos:              gaggleAdditionalRepos[gaggle],
			HarnessInfo:                  harnessInfo,
			CredentialStores:             input.CredentialStores,
			SandboxPosture:               sandboxPostures[gaggle],
			SelfIdentity:                 selfIdentities[gaggle],
			RequireLabelsDefault:         requireLabelsDefaults[gaggle],
			BacklogLabelsDefault:         backlogLabelsDefaults[gaggle],
			BacklogLabelPredicateDefault: backlogLabelPredicateDefaults[gaggle],
			OwnershipAssigneesDefault:    ownershipDefaults.assignees[gaggle],
			OwnershipUnassignedDefault:   ownershipDefaults.unassigned[gaggle],
			Generations:                  []string{generation},
		})
		if err != nil {
			return nil, fmt.Errorf("initialize gaggle %q runtime: %w", gaggle, err)
		}
		input.WorktreeManagers[gaggle] = manager
		runners[gaggle] = rn
		engineHooks[gaggle] = hooks
		reportStartupProgress(input.StartupProgress, fmt.Sprintf("gaggle %q runtime ready", gaggle))
	}

	openPRRefresher, err := buildOpenPRRefresher(input.Config, input.Definitions.Workflows, gaggleProjects, input.SharedRegistry, branchNamespaces, l.SchedulerDir(), input.CredentialStores, generation)
	if err != nil {
		return nil, err
	}
	loc, err := input.Config.Location()
	if err != nil {
		return nil, err
	}
	credResolver, _, err := buildCredentials(input.Config, input.CredentialStores, "", "", nil, input.SharedRegistry)
	if err != nil {
		return nil, err
	}

	gagglesByName := indexedGaggles(input.Definitions)

	// Select each lane's substrate before solving placement: engine lanes do
	// not execute on the daemon host. Selection only reads config and machines.
	selections, err := engineSelections(input.Config, input.Definitions, machines)
	if err != nil {
		return nil, err
	}

	// Checkpoint 3 (#2860, dsl-3.0.md §5): solve every workflow against the
	// declared inventory; an unsatisfiable RUNNER-DRIVEN workflow is marked
	// refused with a named diagnostic — journaled and refused per-run by the
	// scheduler — while the daemon and every other workflow keep serving.
	// Empty on a zero-declaration instance (see placementrefusal.go).
	placement, err := placementRefusals(input.Config, input.Definitions, goobers, machines, selections)
	if err != nil {
		return nil, err
	}
	for _, identity := range sortedWorkflowIdentities(placement.Refusals) {
		fmt.Fprintf(os.Stderr, "warning: workflow %q (gaggle %q) cannot be placed on the declared runners: inventory and is refused: %s\n",
			identity.Workflow, identity.Gaggle, placement.Refusals[identity])
	}
	// An exempted lane's diagnostic is still reported: it names the runners
	// the stage can place on, which is what an operator needs if the engine
	// side of the dispatch later misbehaves. Reported as a note, because
	// nothing is refused.
	for _, identity := range sortedWorkflowIdentities(placement.EngineDeferred) {
		fmt.Fprintf(os.Stderr, "note: workflow %q (gaggle %q) cannot be placed on the daemon's own substrate (%s), but every stage is pinned on the declared inventory and the run dispatches through the engine, so it is not refused\n",
			identity.Workflow, identity.Gaggle, placement.EngineDeferred[identity])
	}
	// The Temporal client and the live journal writer do not exist yet — see
	// engineRuntime. Every engineStarter shares this holder and up.go attaches
	// it once both exist.
	engineRuntimeHolder := &engineRuntime{}

	entries := make([]localscheduler.WorkflowEntry, 0, len(input.Definitions.Workflows))
	for i := range input.Definitions.Workflows {
		wf := &input.Definitions.Workflows[i]
		identity := localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}
		machine := machines[identity]
		// Preview authorization is per-Workflow (#4220): wf's OWN annotations,
		// never the Manifest's or its gaggle's.
		allowPreviewFeatures := workflow.PreviewFeaturesEnabled(wf.Annotations)
		// Collect every schedule and signal subscription for this workflow.
		var scheds []localscheduler.Schedule
		var scheduleBackoffs []localscheduler.IdleBackoffConfig
		webhookBackoff, _ := localscheduler.ParseIdleBackoff(nil)
		var sigs []string
		hasRepositoryWebhook := false
		var pollPriority int32
		pollPrioritySet := false
		for _, trigger := range wf.Spec.Triggers {
			if triggerDisabled(trigger) {
				continue
			}
			if trigger.Type == apiv1.TriggerSchedule && trigger.Schedule != "" {
				schedule, err := localscheduler.ParseSchedule(trigger.Schedule)
				if err != nil {
					return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				scheds = append(scheds, localscheduler.InLocation(schedule, loc))
				backoff, err := localscheduler.ParseIdleBackoff(trigger.IdleBackoff)
				if err != nil {
					return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				scheduleBackoffs = append(scheduleBackoffs, backoff)
			}
			if trigger.Type == apiv1.TriggerSignal && trigger.Signal != "" {
				sigs = append(sigs, trigger.Signal)
			}
			if trigger.Type == apiv1.TriggerWebhook {
				hasRepositoryWebhook = true
				webhookSigs, backoff, err := webhookTriggerSignalsAndBackoff(wf.Name, trigger)
				if err != nil {
					return nil, err
				}
				sigs = append(sigs, webhookSigs...)
				webhookBackoff = backoff
			}
			if trigger.Type == apiv1.TriggerBacklogItem || trigger.Type == apiv1.TriggerSchedule {
				if !pollPrioritySet || trigger.Priority > pollPriority {
					pollPriority = trigger.Priority
					pollPrioritySet = true
				}
			}
		}
		if len(scheds) > 0 {
			project := gaggleProjects[wf.Spec.Gaggle]
			if err := validateScheduledWorkflowCredentialEnvironment(machine, input.Config, project, gaggleBacklogRef(input.Definitions, wf.Spec.Gaggle)); err != nil {
				return nil, err
			}
		}
		pollFallbackCause := ""
		if hasRepositoryWebhook && len(scheds) > 0 {
			switch {
			case repoRefs[identity].Provider != apiv1.ProviderGitHub:
				pollFallbackCause = "repository provider does not support GitHub webhook delivery"
			case !input.Config.WebhookSecretConfigured():
				pollFallbackCause = "webhook listener is disabled because webhook.secret is not configured"
			default:
				pollFallbackCause = "no usable webhook delivery was available"
			}
		}
		// RRQ-1/#1101: the runner capabilities a single run of this workflow
		// needs (its gaggle's + its stages'). The scheduler matches them at
		// dispatch against the runner's advertised set (schedule-time), and the
		// runner preflight-verifies the probeable toolchains among them on the
		// host before any stage runs (#735).
		requiredCaps := selections[identity].starterCapabilities(instance.WorkflowRequiredCapabilities(gagglesByName[wf.Spec.Gaggle], *wf))
		// Shared with `goobers engine-start` so the two starters cannot pin
		// different budgets for the same workflow (#3820).
		controls, err := resolveWorkflowRunControls(input.Config, repoRefs[identity], gagglesByName[wf.Spec.Gaggle], *wf)
		if err != nil {
			return nil, fmt.Errorf("workflow %q run controls: %w", wf.Name, err)
		}
		backlogCounter, err := buildBacklogCounter(input.Config, gagglesByName[wf.Spec.Gaggle], wf, repoRefs[identity], credResolver, input.SharedRegistry, l.SchedulerDir(), input.ProviderQuota, l.Root, generation)
		if err != nil {
			return nil, err
		}
		refillDemandCounter, err := buildRefillDemandCounter(
			input.Config,
			gagglesByName[wf.Spec.Gaggle],
			wf,
			repoRefs[identity],
			credResolver,
			input.SharedRegistry,
			l.SchedulerDir(), selfIdentities[wf.Spec.Gaggle],
			input.ProviderQuota,
			generation,
		)
		if err != nil {
			return nil, err
		}
		entries = append(entries, localscheduler.WorkflowEntry{
			Workflow:            wf.Name,
			WorkflowVersion:     machine.Def.Version,
			WorkflowDigest:      machine.Digest(),
			Gaggle:              wf.Spec.Gaggle,
			Readiness:           wf.Spec.Readiness,
			Schedules:           scheds,
			ScheduleBackoffs:    scheduleBackoffs,
			WebhookBackoff:      webhookBackoff,
			Signals:             sigs,
			PollFallbackCause:   pollFallbackCause,
			BacklogCounter:      backlogCounter,
			RefillDemandCounter: refillDemandCounter,
			ScheduleDemandCounter: buildScheduleDemandCounter(
				input.Config, wf, repoRefs[identity], credResolver, input.SharedRegistry, l.SchedulerDir(),
				branchNamespaces[wf.Spec.Gaggle], input.ProviderQuota, generation,
			),
			// The current provider-backed demand counters use GitHub; charge the
			// provider actually called rather than a future configured adapter.
			PollProvider: apiv1.ProviderGitHub,
			PollPriority: pollPriority,
			Starter: selectEntryStarter(entryStarterInput{
				runnerStarter: &trackedStarter{r: runners[wf.Spec.Gaggle], machine: machine, runControls: controls.Overrides(), requiredCaps: requiredCaps, wg: input.WaitGroup, l: l.ForGaggle(wf.Spec.Gaggle), tel: input.Telemetry, rollupDB: input.RollupDB, watermarks: input.Watermarks, log: input.InstanceLog, runners: input.RunnerRegistry},
				selection:     selections[identity],
				runtime:       engineRuntimeHolder,
				hooks:         engineHooks[wf.Spec.Gaggle],
				gaggle:        wf.Spec.Gaggle,
				def:           machine.Def,
				spec: engineRunRequest{
					configGeneration: generation,
					cfg:              input.Config,
					set:              input.Definitions,
					gaggle:           wf.Spec.Gaggle,
					project:          repoRefs[identity],
					def:              machine.Def,
				},
				layout:               l.ForGaggle(wf.Spec.Gaggle),
				log:                  input.InstanceLog,
				telemetry:            input.Telemetry,
				rollupDB:             input.RollupDB,
				watermarks:           input.Watermarks,
				allowPreviewFeatures: allowPreviewFeatures,
				liveJournal:          input.Config.EngineProjectionEnabled(),
				wg:                   input.WaitGroup,
			}),
			RepoRef: repoRefs[identity],
			// Only runner-driven entries execute on the scheduler's self host.
			// Engine-selected entries enforce capabilities per pinned stage.
			RequiredCapabilities: selections[identity].schedulerSelfCapabilities(requiredCaps),
			DisabledReason:       resolveDisabledReason(gagglesByName[wf.Spec.Gaggle], wf),
			HarnessRefusal:       selections[identity].localHarnessRefusal(harnessRefusals[identity]), // Broken harnesses refuse only their dependent workflows (#5163).
			// Checkpoint 3 (#2860): non-empty exactly when the boot solve
			// above found this workflow unplaceable on the declared inventory
			// AND the entry is runner-driven — an engine-selected entry's
			// placement is proven against the full inventory instead (#3987).
			PlacementRefusal: placement.Refusals[identity],
		})
		entries[len(entries)-1].GooberDigest = gooberDigests[identity]
		entries[len(entries)-1].ConfigGeneration = generation
	}

	firstRunner, firstWorktrees := firstGaggleRuntime(input.Definitions, runners, input.WorktreeManagers)
	eventCatalog, buildGeneration, err := buildPinnedStartMetadata(input, generation, machines, gooberDigests)
	if err != nil {
		return nil, err
	}
	return &schedulerDefinitions{
		GenerationResolver: generationResolverFor(l, firstGenerationRetainer(input.Generations), buildGeneration),
		EventCatalog:       eventCatalog,
		ChildRuntime:       childRuntimeBuilderFor(l, firstGenerationRetainer(input.Generations), input.Config, buildGeneration),
		EventRuntime:       eventRuntimeBuilderFor(l, firstGenerationRetainer(input.Generations), buildGeneration),
		OrdinaryRuntime:    ordinaryRuntimeBuilderFor(l, firstGenerationRetainer(input.Generations), buildGeneration),
		Set:                input.Definitions,
		Validation:         input.Validation,
		HarnessPreflight:   harnessInfo,
		Runner:             firstRunner,
		Runners:            runners,
		Entries:            entries,
		Machines:           machines,
		GooberDigests:      gooberDigests,
		Goobers:            resolvedGoobers,
		RepoRefs:           repoRefs,
		OpenPRRefresher:    openPRRefresher,
		EngineRuntime:      engineRuntimeHolder,
		Worktrees:          firstWorktrees,
		WorktreesByGaggle:  input.WorktreeManagers,
	}, nil
}

func schedulerGenerationBuilder(input schedulerDefinitionsInput) func(instance.Layout, *instance.ConfigSet, *validate.Report) (*schedulerDefinitions, error) {
	return func(pinned instance.Layout, pinnedSet *instance.ConfigSet, pinnedReport *validate.Report) (*schedulerDefinitions, error) {
		pinnedInput := input
		pinnedInput.Layout = pinned
		pinnedInput.Definitions = pinnedSet
		pinnedInput.Validation = pinnedReport
		pinnedInput.StartupProgress = nil
		return buildSchedulerDefinitions(pinnedInput)
	}
}

func preflightSchedulerHarnessesWithProgress(
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	stores credentials.StoreResolver,
	startupProgress func(string),
) (harnessPreflightInfo, map[localscheduler.WorkflowIdentity]string, error) {
	reportStartupProgress(startupProgress, "preflighting agentic harnesses")
	started := time.Now()
	harnessInfo, harnessRefusals, err := preflightSchedulerHarnesses(cfg, set, goobers, stores)
	reportStartupProgress(startupProgress, harnessPreflightCompletionMessage(err, time.Since(started)))
	return harnessInfo, harnessRefusals, err
}

func harnessPreflightCompletionMessage(err error, elapsed time.Duration) string {
	if err != nil {
		return fmt.Sprintf("agentic harness preflight failed (duration %s)", elapsed)
	}
	return fmt.Sprintf("agentic harnesses ready (preflight duration %s)", elapsed)
}

func compileSchedulerMachinesWithProgress(
	l instance.Layout,
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
	modelCredential func(context.Context) (string, error),
	startupProgress func(string),
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, map[string]apiv1.GooberSpec, []gooberHarnessWarning, error) {
	reportStartupProgress(startupProgress, "compiling workflow machines")
	return compiledMachinesWithGooberDigestsAndWarnings(
		l.ConfigDir(), set, goobers, instructions, harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand,
		true, modelCredential, cfg.ExternalTelemetryConnectorNames(),
	)
}

func validateScheduledWorkflowCredentialEnvironment(machine *workflow.Machine, cfg *instance.Config, project apiv1.RepoRef, backlog apiv1.BacklogRef) error {
	envByCapability, err := scheduledWorkflowCredentialEnvironments(cfg, project, backlog)
	if err != nil {
		return err
	}
	required := staticallyRequiredWorkflowStates(machine.Graph())
	for _, task := range machine.Def.Spec.Tasks {
		if !required[task.Name] {
			continue
		}
		for _, capability := range task.Capabilities {
			env, credentialed := envByCapability[capability]
			if !credentialed {
				continue
			}
			value, set := os.LookupEnv(env)
			switch {
			case !set:
				return fmt.Errorf("workflow %q cannot be scheduled: credential capability %q requires environment variable %q, which is not set", machine.Def.Name, capability, env)
			case strings.TrimSpace(value) == "":
				return fmt.Errorf("workflow %q cannot be scheduled: credential capability %q requires environment variable %q, which is empty", machine.Def.Name, capability, env)
			}
		}
	}
	return nil
}

func scheduledWorkflowCredentialEnvironments(cfg *instance.Config, project apiv1.RepoRef, backlog apiv1.BacklogRef) (map[string]string, error) {
	bindings := make([]credentials.RepoBinding, 0, len(cfg.Repos))
	envByRef := make(map[string]string, len(cfg.Repos)+len(cfg.Credentials)+1)
	for _, repo := range cfg.Repos {
		owner := repo.Owner
		if repo.Provider == string(apiv1.ProviderADO) && repo.Project != "" {
			owner += "/" + repo.Project
		}
		ref := owner + "/" + repo.Name
		tokenRef := ""
		if repo.Token.Configured() || repo.GitHubAppAuth() {
			tokenRef = ref
		}
		if repo.Token.Env != "" {
			envByRef[ref] = repo.Token.Env
		} else if repo.GitHubAppAuth() && repo.Auth.PrivateKey != nil && repo.Auth.PrivateKey.Env != "" {
			envByRef[ref] = repo.Auth.PrivateKey.Env
		}
		bindings = append(bindings, credentials.RepoBinding{Owner: owner, Name: repo.Name, TokenRef: tokenRef})
	}

	role := gaggleBacklogRole(project, backlog)
	overrides := make([]credentials.Grant, 0, len(daemonIdentityCapabilities)+len(cfg.Credentials))
	if cfg.DaemonIdentity != nil {
		overrides = append(overrides, daemonIdentityOverrides(role)...)
		if cfg.DaemonIdentity.Token != nil && cfg.DaemonIdentity.Token.Env != "" {
			envByRef[daemonIdentityRefName] = cfg.DaemonIdentity.Token.Env
		} else if cfg.DaemonIdentity.GitHubApp() && cfg.DaemonIdentity.PrivateKey != nil && cfg.DaemonIdentity.PrivateKey.Env != "" {
			envByRef[daemonIdentityRefName] = cfg.DaemonIdentity.PrivateKey.Env
		}
	}
	for _, grant := range cfg.Credentials {
		key, err := credentialGrantKey(grant)
		if err != nil {
			return nil, fmt.Errorf("build scheduled workflow credential preflight: %w", err)
		}
		ref := credentialRefName(key)
		overrides = append(overrides, credentials.Grant{Capability: key, Ref: ref})
		if grant.Token.Env != "" {
			envByRef[ref] = grant.Token.Env
		}
	}

	owner := project.Owner
	if project.Provider == apiv1.ProviderADO && project.Project != "" {
		owner += "/" + project.Project
	}
	grants := withoutNonADORepoGrants(cfg.Repos, credentials.RunnerGrants(bindings, owner, project.Name, role, repoCredentialedCapabilityNames(), overrides))
	envByCapability := make(map[string]string, len(grants))
	for _, grant := range grants {
		if env := envByRef[grant.Ref]; env != "" {
			envByCapability[grant.Capability] = env
		}
	}
	return envByCapability, nil
}

func staticallyRequiredWorkflowStates(graph workflow.Graph) map[string]bool {
	outgoing := make(map[string][]workflow.GraphEdge, len(graph.Nodes))
	parallel := make(map[string]bool, len(graph.Nodes))
	for _, edge := range graph.Edges {
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
	}
	for _, node := range graph.Nodes {
		parallel[node.ID] = node.Kind == workflow.GraphNodeParallel
	}
	required := make(map[string]bool, len(graph.Nodes))
	for _, candidate := range graph.Nodes {
		canFinish := make(map[string]bool, len(graph.Nodes))
		changed := true
		for changed {
			changed = false
			for _, node := range graph.Nodes {
				if node.ID == candidate.ID || canFinish[node.ID] {
					continue
				}
				edges := outgoing[node.ID]
				if parallel[node.ID] {
					hasBranches := false
					allBranchesFinish := true
					for _, edge := range edges {
						if edge.Branch == "" {
							if edge.Terminal != "" || canFinish[edge.Target] {
								canFinish[node.ID] = true
								changed = true
								break
							}
							continue
						}
						hasBranches = true
						if !canFinish[edge.Target] {
							allBranchesFinish = false
						}
					}
					if !canFinish[node.ID] && hasBranches && allBranchesFinish {
						canFinish[node.ID] = true
						changed = true
					}
					continue
				}
				for _, edge := range edges {
					if edge.Terminal != "" || canFinish[edge.Target] {
						canFinish[node.ID] = true
						changed = true
						break
					}
				}
			}
		}
		required[candidate.ID] = !canFinish[graph.Start]
	}
	return required
}

func buildRetainedLegacyRunner(input retainedLegacyRunnerInput) (*runner.Runner, *worktree.Manager, error) {
	retained, err := retainedLegacyRuntimeExists(input.Layout)
	if err != nil || !retained {
		return nil, nil, err
	}
	// Legacy retained runtime: no per-gaggle project scoping — a zero project
	// repo leaves credentials on the first-repo default (unchanged behavior).
	instructions, err := loadGooberInstructions(input.Layout.ConfigDir(), input.Goobers)
	if err != nil {
		return nil, nil, err
	}
	rn, manager, _, err := buildRuntimeRunner(runtimeRunnerInput{
		Definitions:          input.Definitions,
		Layout:               input.Layout,
		Config:               input.Config,
		Goobers:              input.Goobers,
		InstructionsByGoober: instructions,
		Telemetry:            input.Telemetry,
		InstanceLog:          input.InstanceLog,
		SharedRegistry:       input.SharedRegistry,
		WorktreeManager:      nil,
		ProviderQuota:        input.ProviderQuota,
		Watermarks:           input.Watermarks,
		TerminalNotifier:     input.TerminalNotifier,
		BranchNamespaces:     branchNamespacesByGaggle(input.Definitions),
		GaggleProject:        apiv1.RepoRef{},
		GaggleBacklog:        apiv1.BacklogRef{},
		AdditionalRepos:      nil,
		HarnessInfo:          input.HarnessInfo,
		CredentialStores:     input.CredentialStores,
		// Legacy retained runtime is not gaggle-scoped, so only the
		// instance-wide posture can apply (no gaggle override to consult).
		SandboxPosture: instance.EffectiveAgenticSandbox(input.Config, nil),
		SelfIdentity:   instance.EffectiveSelfIdentity(input.Config, nil),
		// Same reasoning: no gaggle to consult for a RequireLabels default.
		RequireLabelsDefault:         "",
		BacklogLabelsDefault:         "",
		BacklogLabelPredicateDefault: "",
		OwnershipAssigneesDefault:    "",
		OwnershipUnassignedDefault:   "",
	})
	return rn, manager, err
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

func buildRuntimeRunner(input runtimeRunnerInput) (*runner.Runner, *worktree.Manager, *engineTerminalHooks, error) {
	manager := input.WorktreeManager

	appliedConfigDigest, err := deterministicStageConfigDigest(input.Layout.ConfigDir(), input.Layout.Gaggle())
	if err != nil {
		return nil, nil, nil, err
	}
	var generation string
	if len(input.Generations) > 0 {
		generation = input.Generations[0]
	}
	runnerCfg, manager, err := buildRunnerConfig(runnerCompositionInput{
		ConfigGeneration:     generation,
		Layout:               input.Layout,
		Config:               input.Config,
		Goobers:              input.Goobers,
		InstructionsByGoober: input.InstructionsByGoober,
		Telemetry:            input.Telemetry,
		SharedRegistry:       input.SharedRegistry,
		WorktreeManager:      manager,
		BranchNamespaces:     input.BranchNamespaces,
		GaggleProject:        input.GaggleProject,
		GaggleBacklog:        input.GaggleBacklog,
		AdditionalRepos:      input.AdditionalRepos,
		HarnessInfo:          input.HarnessInfo,
		CredentialStores:     input.CredentialStores,
		SandboxPosture:       input.SandboxPosture,
		ProviderQuota:        input.ProviderQuota,
		AppliedConfigDigest:  appliedConfigDigest,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	runnerCfg.BacklogQueryAssignedTo = input.SelfIdentity
	// The daemon owns root identity creation; tier-3 workers must not create
	// independent identities while loading a copied configuration tree.
	runnerCfg.InstanceID, err = input.Layout.EnsureIdentity(context.Background())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("initialize daemon instance identity: %w", err)
	}
	runnerCfg.BacklogQueryRequireLabels = input.RequireLabelsDefault
	runnerCfg.BacklogQueryBacklogLabels = input.BacklogLabelsDefault
	runnerCfg.BacklogQueryLabelPredicate = input.BacklogLabelPredicateDefault
	runnerCfg.IssueOwnershipAssignees = input.OwnershipAssigneesDefault
	runnerCfg.IssueOwnershipUnassigned = input.OwnershipUnassignedDefault
	runnerCfg.JournalAdvancedContext = telemetryingest.RunIntakeObserverContext(input.Watermarks, input.InstanceLog)
	prepareTerminal, err := buildTerminalBranchPreparer(input.Layout, input.Config, input.GaggleProject, input.SharedRegistry, input.CredentialStores)
	if err != nil {
		return nil, nil, nil, err
	}
	runnerCfg.PrepareTerminal = prepareTerminal.runnerPreparer()
	// #3347: retire the provider-visible claim marker in the same terminal
	// cleanup step that releases the ledger lease, so a run that never reaches
	// issue-close-out (the `no-work` outcome short-circuits straight to
	// completed) cannot leave claims.json and the provider disagreeing until
	// the next backlog-curation cycle.
	releaseClaimMarker, claimMarkerRepo, err := buildTerminalClaimMarkerRelease(input.Layout, input.Config, input.GaggleProject, input.SharedRegistry, input.CredentialStores)
	if err != nil {
		return nil, nil, nil, err
	}
	runnerCfg.FinalizeTerminal = func(runID string, _ journal.RunPhase) error {
		return finalizeTerminalRunWithClaimMarkers(input.Layout, input.InstanceLog, manager, runID, claimMarkerRepo, releaseClaimMarker)
	}
	runnerCfg.RateLimited = buildRateLimitedHandler(input.ProviderQuota)
	runnerCfg.NotifyTerminal = composeTerminalNotifier(runnerCfg.NotifyTerminal, input.TerminalNotifier)
	childHandoff := &daemonChildHandoff{layout: input.Layout, worktrees: manager, repoCloneURL: runnerCfg.RepoCloneURL, project: input.GaggleProject}
	runnerCfg.ChildHandoff, runnerCfg.ChildParentCapacity = childHandoff, childHandoff
	runnerCfg = withContainedParentExecutor(runnerCfg, input.Layout.Root, input.Config, input.Definitions)
	rn, err := runner.New(runnerCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	// #3876: the engine terminal-hook frame is derived from THIS config, not
	// rebuilt. Every field below is the identical closure the local runner
	// will call, so a run that ends on the engine has the same instance-level
	// consequences as the same run ending on the runner — which is the whole
	// claim D1's parity rests on.
	hooks := &engineTerminalHooks{
		selfExecutionObserved: runnerCfg.SelfExecutionObserved,
		layout:                input.Layout,
		log:                   input.InstanceLog,
		repoRef:               input.GaggleProject,
		existingFix:           runnerCfg.ExistingFix,
		blocked:               runnerCfg.Blocked,
		failed:                runnerCfg.Failed,
		escalation:            runnerCfg.Escalation,
		claimedItems:          runnerCfg.ClaimedItems,
		prepare:               prepareTerminal,
		notify:                runnerCfg.NotifyTerminal,
		finalize:              runnerCfg.FinalizeTerminal,
		attribute:             creditgraph.WriteRunRecord,
	}
	return rn, manager, hooks, nil
}

// composeTerminalNotifier chains the instance circuit breaker ahead of the
// terminal notifier and joins both errors instead of discarding the breaker's
// (#3646). A failed park — the streak update or the goobers:ready →
// goobers:needs-human swap — is exactly the failure the run journal has to
// record as terminal_notification_failed, so it must reach the runner rather
// than being swallowed at this wiring boundary. Both hooks always run: a
// breaker failure must not suppress the terminal notification.
func composeTerminalNotifier(circuitBreaker, terminalNotifier runner.TerminalNotifier) runner.TerminalNotifier {
	if circuitBreaker == nil {
		return terminalNotifier
	}
	if terminalNotifier == nil {
		return circuitBreaker
	}
	return func(runID string, phase journal.RunPhase, finalState string) error {
		breakerErr := circuitBreaker(runID, phase, finalState)
		notifyErr := terminalNotifier(runID, phase, finalState)
		return errors.Join(breakerErr, notifyErr)
	}
}

func configuredGaggleNames(set *instance.ConfigSet) []string {
	names := make([]string, 0, len(set.Gaggles))
	for i := range set.Gaggles {
		names = append(names, set.Gaggles[i].Name)
	}
	sort.Strings(names)
	return names
}

func resolveDisabledReason(gaggle apiv1.Gaggle, wf *apiv1.Workflow) string {
	if gaggle.Spec.Enabled != nil && !*gaggle.Spec.Enabled {
		return fmt.Sprintf("gaggle %q is disabled (spec.enabled=false)", gaggle.Name)
	}
	if wf != nil && wf.Spec.Enabled != nil && !*wf.Spec.Enabled {
		return fmt.Sprintf("workflow %q is disabled (spec.enabled=false)", wf.Name)
	}
	return ""
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
	opts := []localscheduler.Option{localscheduler.WithProviderQuota(s.ProviderQuota), localscheduler.WithSourceQueue(s.SourceStarts)}
	if s.Root != "" {
		opts = append(opts, localscheduler.WithTargetedPRValidator(func(ctx context.Context, entry localscheduler.WorkflowEntry, number int) error {
			return validateTargetedPullRequest(ctx, s.Root, s.Config, s.SecretStores, s.SharedRegistry, entry, number)
		}))
	}
	// RRQ-1/#1101: the local runner's static advertised capability set, so
	// dispatch can refuse a run whose gaggle/stages require a capability this
	// runner does not claim. Wired uniformly for both `up` and `run`.
	if s.Config != nil {
		opts = append(opts, localscheduler.WithRunnerCapabilities(s.Config.SelfRunnerCapabilities()))
	}
	if s.Telemetry != nil {
		opts = append(opts, localscheduler.WithTelemetry(s.Telemetry))
		if s.RollupDB != nil && s.InstanceLog != nil {
			opts = append(opts, localscheduler.WithAfterTick(func(ctx context.Context) {
				telemetryingest.SchedulerTelemetry(ctx, s.Telemetry, s.RollupDB, s.InstanceLog.Dir(), s.InstanceLog)
			}))
		}
	}
	if s.Telemetry != nil && s.RollupDB != nil {
		opts = append(opts, localscheduler.WithAfterTick(func(ctx context.Context) {
			if err := s.Telemetry.Flush(ctx); err != nil {
				telemetryingest.LogFailure(s.InstanceLog, "", "telemetry_flush_scheduler_failed", err)
			}
			_ = s.ingestSchedulerLog(context.Background())
		}))
	}
	return opts
}

func (s *schedulerSetup) ingestSchedulerLog(ctx context.Context) error {
	if s.RollupDB == nil || s.InstanceLog == nil {
		return nil
	}
	if err := s.RollupDB.IngestSchedulerLog(ctx, s.InstanceLog.Dir()); err != nil {
		telemetryingest.LogFailure(s.InstanceLog, "", "telemetry_ingest_scheduler_log_failed", err)
		return err
	}
	return nil
}

// schedulerShutdownGrace bounds a setup shutdown that was handed an unbounded
// context (#3651). Telemetry export and the sqlite closes below can all block;
// without a deadline a daemon stop hangs forever instead of reporting which
// step is stuck.
var schedulerShutdownGrace = 30 * time.Second

// shutdownStep is one named close/flush in a setup shutdown. The name is what
// makes a timeout actionable: it says which resource was still closing.
type shutdownStep struct {
	name string
	run  func() error
}

// Shutdown ingests any final scheduler spans while the telemetry client can
// still observe a diagnostic append failure, then flushes/closes telemetry and
// closes the rollup db, read model, watermarks, and instance log.
// It is nil-safe so a caller can defer it unconditionally regardless of
// whether instance.yaml enabled telemetry (issue #129), it is bounded so a
// wedged flush cannot hang the process, and it joins every step's error so a
// caller never reports a clean shutdown after losing final persisted state
// (#3651). It runs at most once; later calls return the first call's result.
func (s *schedulerSetup) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, schedulerShutdownGrace)
		defer cancel()
	}
	s.shutdownOnce.Do(func() {
		s.shutdownErr = runShutdownSteps(ctx, s.shutdownSteps(ctx))
	})
	return s.shutdownErr
}

// testInjectedShutdownStepErr is a test-only seam (#3851): when set, an
// extra shutdown step returning this error is appended so command-level
// tests can exercise a real Shutdown failure end-to-end — through `run` and
// `signal`'s full instance/scheduler setup — without contriving a genuine
// telemetry-flush or store-close failure. Nil in production.
var testInjectedShutdownStepErr error

func (s *schedulerSetup) shutdownSteps(ctx context.Context) []shutdownStep {
	var steps []shutdownStep
	if testInjectedShutdownStepErr != nil {
		steps = append(steps, shutdownStep{"test-injected failure", func() error { return testInjectedShutdownStepErr }})
	}
	// The rollup ingests spans from the local journal exporter, so drain that
	// exporter before the final scheduler scan without shutting down metrics.
	if s.Telemetry != nil {
		steps = append(steps, shutdownStep{"telemetry local flush", func() error { return s.Telemetry.FlushLocal(ctx) }})
	}
	if s.RollupDB != nil {
		steps = append(steps, shutdownStep{"scheduler telemetry ingest", func() error { return s.ingestSchedulerLog(ctx) }})
	}
	observation := s.observation
	if observation == nil {
		// Compatibility for callers assembling a setup directly.
		observation = &schedulerObservation{tel: s.Telemetry, rollupDB: s.RollupDB,
			stopProjector: s.StopProjector, readModel: s.ReadModel, watermarks: s.Watermarks, instanceLog: s.InstanceLog}
	}
	steps = append(steps, observation.closeSteps(ctx)...)
	runtime := s.runtime
	if runtime == nil {
		runtime = &schedulerRuntime{generations: s.Generations}
	}
	steps = append(steps, shutdownStep{"config generation leases", runtime.Close})

	return steps
}

// runShutdownSteps runs steps in order on a goroutine so ctx can bound the
// whole sequence. On timeout the in-flight step keeps running — a close cannot
// be cancelled — but the caller is freed and told which step wedged rather
// than blocking on it forever.
func runShutdownSteps(ctx context.Context, steps []shutdownStep) error {
	var pending atomic.Value
	pending.Store("")
	done := make(chan error, 1)
	go func() {
		var errs []error
		for _, step := range steps {
			pending.Store(step.name)
			if err := step.run(); err != nil {
				errs = append(errs, fmt.Errorf("shut down %s: %w", step.name, err))
			}
		}
		pending.Store("")
		done <- errors.Join(errs...)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		stuck, _ := pending.Load().(string)
		if stuck == "" {
			stuck = "unknown step"
		}
		return fmt.Errorf("shutdown timed out while closing %s: %w", stuck, ctx.Err())
	}
}

// trackedStarter adapts a *runner.Runner + its compiled Machine into a
// localscheduler.Starter — one per workflow, per that seam's doc comment
// ("#17's *runner.Runner is bound to a single compiled machine at
// construction, so the scheduler holds a map of workflow name -> Starter").
// It also tracks every dispatched run in wg so the daemon's shutdown drain
// (runUpContext) waits for scheduler-dispatched runs, not just the startup
// resume scan's. The scheduler calls RegisterDispatch before launching its
// dispatch goroutine, so shutdown can wait on wg without a registration race.
// Every dispatch through this Starter — both
// `goobers up`'s scheduled/manual-via-Trigger fires and `goobers run`'s own
// sched.Trigger call, now that #134 routes it through the same scheduler —
// incrementally ingests into rollupDB on completion (issue #127).
type trackedStarter struct {
	starterSelection map[string]any
	r                *runner.Runner
	machine          *workflow.Machine
	runControls      apiv1.RunControls
	requiredCaps     []string
	wg               *sync.WaitGroup
	l                instance.Layout
	tel              *telemetry.Client
	rollupDB         *rollup.DB
	watermarks       *intake.Store
	log              *journal.InstanceLog
	runners          *daemonRunnerRegistry
}

func (s *trackedStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	untrack := s.runners.Track(req.RunID, s.machine.Def.Name, s.r)
	defer untrack()
	res, err := s.r.Start(ctx, runner.StartInput{
		StarterSelection:     s.starterSelection,
		RunID:                req.RunID,
		Machine:              s.machine,
		GooberDigest:         req.GooberDigest,
		Gaggle:               req.Gaggle,
		Trigger:              req.Trigger,
		RepoRef:              req.RepoRef,
		Item:                 req.Item,
		EventInputs:          req.EventInputs,
		RunControls:          s.runControls,
		RequiredCapabilities: s.requiredCaps,
	})
	telemetryingest.RunTelemetry(s.tel, s.rollupDB, s.watermarks, s.l, req.RunID, s.log)
	return localscheduler.StartResult{
		Phase:          res.Phase,
		FinalState:     res.FinalState,
		NoWork:         res.NoWork,
		FailureStage:   res.FailureStage,
		FailureCode:    res.FailureCode,
		FailureMessage: res.FailureMessage,
	}, err
}

func (s *trackedStarter) RegisterDispatch() func() {
	if s.wg == nil {
		return func() {}
	}
	s.wg.Add(1)
	return s.wg.Done
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
// A run whose run.yaml names a driver other than this process's runner
// (journal.RunIdentity.EngineDriven) is never resumed: it is re-attached
// instead — the scan journals a run.recovery annotation with action
// "reattached" and hands the run to reattachEngineRun, which waits on the
// engine's workflow. See enginerunguards.go for why "resume it anyway" is a
// duplicate-driver bug rather than a redundant safety net.
//
// resumeInterruptedRuns errors when the scan itself cannot proceed or when
// terminal-run cleanup fails. A deferred durable handoff is journaled and
// retried later without blocking daemon readiness; all other cleanup failures
// remain fatal.
func resumeInterruptedRuns(ctx context.Context, l instance.Layout, rn *runner.Runner, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, gooberDigests map[localscheduler.WorkflowIdentity]string, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, watermarks *intake.Store, release func(runID, workflow string), wg *sync.WaitGroup) (resumed []string, warned []string, err error) {
	outcome, err := resumeInterruptedRunsWithRunners(ctx, l, nil, rn, nil, nil, machines, gooberDigests, repoRefs, log, tel, rollupDB, watermarks, release, wg, nil)
	if err != nil {
		return outcome.Resumed, outcome.Warned, err
	}
	// This one-shot path has no readiness to protect and no drain to survive,
	// so its terminal finalizations stay inline and fatal, exactly as before
	// #5199.
	finalizer := &startupTerminalFinalizer{remaining: outcome.Terminal, log: log, watermarks: watermarks}
	return outcome.Resumed, outcome.Warned, finalizer.run(ctx, nil)
}

func interruptedRunMachine(id journal.RunIdentity, current *workflow.Machine) (*workflow.Machine, string) {
	if id.WorkflowDigest != "" && current.Digest() != id.WorkflowDigest {
		return nil, "pinned-snapshot"
	}
	return current, "current-config"
}

func resumeInterruptedRunsWithRunners(ctx context.Context, l instance.Layout, runners map[string]*runner.Runner, fallback *runner.Runner, runnerRegistry *daemonRunnerRegistry, guards *engineRunGuards, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, gooberDigests map[localscheduler.WorkflowIdentity]string, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, watermarks *intake.Store, release func(runID, workflow string), wg *sync.WaitGroup, progress resumeProgressFunc, recoveryRunDirs ...[]string) (outcome resumeOutcome, err error) {
	candidates, err := recoveryRunCandidates(ctx, l, recoveryRunDirs...)
	if err != nil {
		return resumeOutcome{}, err
	}
	outcome.Total = len(candidates)
	// A closure, not `defer outcome.report(progress)`: the latter evaluates
	// the receiver at defer time, reporting the zero outcome.
	defer func() { outcome.report(progress) }()
	for _, dir := range candidates {
		outcome.Examined++
		outcome.report(progress)
		runsDir := filepath.Dir(dir)
		runName := filepath.Base(dir)
		rd, err := journal.OpenRead(dir)
		if err != nil {
			if errors.Is(err, journal.ErrNotRunDirectory) {
				continue
			}
			return outcome, fmt.Errorf("open run journal %q: %w", runName, err)
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
				// #5199: a terminal run has nothing to RESUME, and its
				// cleanup is not what scheduling waits on. Finalization is
				// the expensive half of this loop — a worktree FinalizeRun,
				// a claim-ledger release, and a full recovery-inventory read
				// per run — so it is collected and run after readiness
				// instead of ahead of it. The concurrency slot is released
				// here, synchronously: ReconcileRunDirs seeded one for every
				// candidate moments ago, and a scheduler that believes those
				// slots are still held cannot dispatch anything.
				outcome.Terminal = append(outcome.Terminal, terminalFinalization{
					layout: runLayout, runsDir: runsDir, runner: rn, identity: id, phase: phase,
				})
				release(id.RunID, id.Workflow)
				continue // terminal: nothing to resume
			}
		}

		// Engine-driven runs are re-attached, never resumed. Every WF-016
		// check Runner.Resume applies passes on an engine-authored
		// journal — the pinned definition, digest and inputs are all
		// there — so without this branch a goobers-api restart during an
		// engine run walks it a SECOND time in-process while the worker
		// keeps walking it on Temporal: two drivers, two open-pr /
		// push-branch / merge-pr attempts, one journal. The daemon's job
		// here is not to drive the run but to stop pretending it can.
		if id.EngineDriven() {
			outcome.Reattached = append(outcome.Reattached, id.RunID)
			if log != nil {
				if err := log.Append(journal.Event{
					Type: journal.EventRunnerAnnotation, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
					Runner: map[string]any{
						"kind":   journal.RunnerAnnotationRunRecovery,
						"reason": "daemon_restart",
						"action": journal.RecoveryActionReattached,
						"driver": string(id.Driver),
					},
				}); err != nil {
					return outcome, fmt.Errorf("journal engine re-attachment for run %q: %w", id.RunID, err)
				}
			}
			// Deliberately outside wg and outside the runner registry: see
			// reattachEngineRun. Waiting for another process's run would
			// hold this daemon's SIGTERM drain open for that run's whole
			// duration, and hard-stopping it is not even meaningful.
			go reattachEngineRun(ctx, guards, id, engineReattachDeps{
				layout:     runLayout,
				log:        log,
				telemetry:  tel,
				rollupDB:   rollupDB,
				watermarks: watermarks,
				release:    release,
			})
			continue
		}

		runtime, available, err := resolveInterruptedRuntime(ctx, id, interruptedRuntimeInput{
			runner: rn, registry: runnerRegistry, machines: machines, gooberDigests: gooberDigests,
			repoRefs: repoRefs, log: log, release: release,
		})
		if err != nil {
			return outcome, err
		}
		if !available {
			outcome.Warned = append(outcome.Warned, id.RunID)
			continue
		}
		rn, machine, gooberDigest, repoRef := runtime.runner, runtime.machine, runtime.gooberDigest, runtime.repoRef
		// Never reinterpret a historical run under the current workflow
		// merely because the name still matches.
		machine, machineSource := interruptedRunMachine(id, machine)

		outcome.Resumed = append(outcome.Resumed, id.RunID)
		if log != nil {
			if err := log.Append(journal.Event{
				Type: journal.EventRunnerAnnotation, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
				Runner: map[string]any{
					"kind":                     journal.RunnerAnnotationRunRecovery,
					"reason":                   "daemon_restart",
					"action":                   journal.RecoveryActionResumed,
					"workflowDigest":           id.WorkflowDigest,
					"workflowDefinitionSource": machineSource,
				},
			}); err != nil {
				return outcome, fmt.Errorf("journal recovery for run %q: %w", id.RunID, err)
			}
		}
		wg.Add(1)
		untrack := runnerRegistry.Track(id.RunID, id.Workflow, rn)
		go func(runID, gaggle, wfName, gooberDigest string, rn *runner.Runner, runLayout instance.Layout, untrack func()) {
			defer wg.Done()
			defer release(runID, wfName)
			defer untrack()
			result, err := rn.Resume(ctx, runner.ResumeInput{
				RunID: runID, Machine: machine, GooberDigest: gooberDigest, RepoRef: repoRef,
				RecoveryReason: "daemon_restart",
			})
			telemetryingest.RunTelemetry(tel, rollupDB, watermarks, runLayout, runID, log)
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
				log.AppendBestEffort(ev)
			}
		}(id.RunID, id.Gaggle, id.Workflow, gooberDigest, rn, runLayout, untrack)
	}
	return outcome, nil
}

// buildReadModelIfNeeded performs the first-start or migration-triggered build
// (design §6.6 step 2).
//
// Readiness is persisted in the store so an interrupted build cannot expose a
// partial projection on the next startup merely because it wrote some rows.
//
// A failure is not fatal: the store remains detached and requests fall back to
// the journal-derived path.
func buildReadModelIfNeeded(ctx context.Context, store *readmodel.Store, state readmodel.State, l instance.Layout) error {
	if state.Ready {
		return nil
	}
	// Startup-only reconstruction must not observe the daemon's lifetime
	// cancellation. This work is not request-scoped and is intentionally not
	// allowed to fail a daemon that is merely shutting down while the first
	// build is still finishing.
	startupCtx := context.Background()
	roots, err := l.RunDirs()
	if err != nil {
		return err
	}
	if _, err := store.BuildFromJournals(startupCtx, roots); err != nil {
		return err
	}
	return store.MarkReady(startupCtx)
}

// bootstrapAndDigestConfigDir seeds a first-boot config tree when one is owed
// (#3314) and returns the digest of the tree that results.
//
// The two are paired here rather than at the call site because the caller is
// already at the complexity gate's ceiling, and because they are one step: the
// digest must describe the tree the daemon will actually validate, which on a
// first boot is the seeded one.
func bootstrapAndDigestConfigDir(l instance.Layout, cfg *instance.Config) (string, error) {
	if err := ensureBootstrapConfigDir(l, cfg); err != nil {
		return "", err
	}
	return configDirectoryDigest(l.ConfigDir())
}

// ensureBootstrapConfigDir seeds a config directory on a first boot whose
// config will arrive from a remote workflowSource (#3314).
//
// The bootstrap order the daemon needs is credentials -> fetch -> validate ->
// serve, and the missing link was that validation ran against a tree the fetch
// had not created yet. `goobers up` failed with "walk /var/lib/goobers/config:
// no such file or directory", and `goobers apply` — the one-shot reconcile that
// would populate it — requires the live daemon that was refusing to start. The
// component that fetches config needed the daemon, and the daemon needed the
// config it had not fetched.
//
// Creating the directory is not enough on its own, and finding that out is the
// point: an empty tree fails validation with exactly one error, CFG002 "no
// Manifest object found in config directory". So the seed is a minimal
// zero-gaggle Manifest — the same artifact operators were hand-writing to get
// past this, now shipped so an adopter following the docs does not have to
// invent it. The daemon already accepts a zero-gaggle instance, and the first
// reconcile replaces the seed with the tracked tree.
//
// Deliberately narrow, in three ways:
//
//   - Only for a git workflowSource. With no remote source configured nothing
//     would ever populate the tree, so a missing config directory stays a hard
//     startup failure: a daemon serving an empty instance forever is worse than
//     one that says why it will not start.
//   - Only when the directory is ABSENT. An existing tree, even an invalid or
//     empty one, is left exactly as it is — this must never overwrite config an
//     operator or a previous sync put there.
//   - The seed names no gaggle, so it cannot schedule anything before the real
//     config arrives.
func ensureBootstrapConfigDir(l instance.Layout, cfg *instance.Config) error {
	if cfg == nil || cfg.WorkflowSource == nil || cfg.WorkflowSource.Kind != instance.WorkflowSourceKindGit {
		return nil
	}
	dir := l.ConfigDir()
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect config directory before first workflow-source sync: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory for first workflow-source sync: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(bootstrapManifestSeed), 0o644); err != nil {
		return fmt.Errorf("seed manifest for first workflow-source sync: %w", err)
	}
	return nil
}

// bootstrapManifestSeed is the smallest config tree that validates: a Manifest
// naming no gaggles, replaced by the first workflow-source sync (#3314).
const bootstrapManifestSeed = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: bootstrap
spec:
  instance:
    name: bootstrap
    environment: dev
  gaggles: []
`

func firstGaggleRuntime(set *instance.ConfigSet, runners map[string]*runner.Runner, managers map[string]*worktree.Manager) (*runner.Runner, *worktree.Manager) {
	for _, gaggle := range configuredGaggleNames(set) {
		return runners[gaggle], managers[gaggle]
	}
	return nil, nil
}
func indexedGaggles(set *instance.ConfigSet) map[string]apiv1.Gaggle {
	out := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for _, gaggle := range set.Gaggles {
		out[gaggle.Name] = gaggle
	}
	return out
}

func clonedWorktreeManagers(managers map[string]*worktree.Manager) map[string]*worktree.Manager {
	out := maps.Clone(managers)
	if out == nil {
		out = make(map[string]*worktree.Manager)
	}
	return out
}

func warnUnresolvableResume(log *journal.InstanceLog, id journal.RunIdentity, missingRunner bool) {
	if log != nil {
		code := "resume_unresolvable_workflow"
		message := fmt.Sprintf("run %q references unknown workflow %q — recover with `goobers run abort %s`", id.RunID, id.Workflow, id.RunID)
		if missingRunner {
			code = "resume_unresolvable_gaggle"
			message = fmt.Sprintf("run %q references inactive gaggle %q — recover with `goobers run abort %s`", id.RunID, id.Gaggle, id.RunID)
		}
		log.AppendBestEffort(journal.Event{
			Type: journal.EventError, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID,
			Error: &journal.ErrorDetail{
				Code:    code,
				Message: message,
			},
		})
	}
}

// schedulerDefinitionsInput names the dependencies for this construction boundary.
type schedulerDefinitionsInput struct {
	Layout           instance.Layout
	Config           *instance.Config
	Definitions      *instance.ConfigSet
	Validation       *validate.Report
	WaitGroup        *sync.WaitGroup
	RunnerRegistry   *daemonRunnerRegistry
	Telemetry        *telemetry.Client
	RollupDB         *rollup.DB
	Watermarks       *intake.Store
	InstanceLog      *journal.InstanceLog
	SharedRegistry   *journal.RegistryScrubber
	WorktreeManagers map[string]*worktree.Manager
	ProviderQuota    *localscheduler.ProviderQuotaState
	TerminalNotifier runner.TerminalNotifier
	CredentialStores credentials.StoreResolver
	StartupProgress  func(string)
	Generations      []*configgeneration.Retainer
}

// retainedLegacyRunnerInput names the dependencies for this construction boundary.
type retainedLegacyRunnerInput struct {
	Layout           instance.Layout
	Config           *instance.Config
	Definitions      *instance.ConfigSet
	Goobers          map[string]apiv1.GooberSpec
	Telemetry        *telemetry.Client
	InstanceLog      *journal.InstanceLog
	SharedRegistry   *journal.RegistryScrubber
	ProviderQuota    *localscheduler.ProviderQuotaState
	Watermarks       *intake.Store
	TerminalNotifier runner.TerminalNotifier
	HarnessInfo      harnessPreflightInfo
	CredentialStores credentials.StoreResolver
}

// runtimeRunnerInput names the dependencies for this construction boundary.
type runtimeRunnerInput struct {
	Definitions                  *instance.ConfigSet
	Layout                       instance.Layout
	Config                       *instance.Config
	Goobers                      map[string]apiv1.GooberSpec
	InstructionsByGoober         map[string]string
	Telemetry                    *telemetry.Client
	InstanceLog                  *journal.InstanceLog
	SharedRegistry               *journal.RegistryScrubber
	WorktreeManager              *worktree.Manager
	ProviderQuota                *localscheduler.ProviderQuotaState
	Watermarks                   *intake.Store
	TerminalNotifier             runner.TerminalNotifier
	BranchNamespaces             map[string]string
	GaggleProject                apiv1.RepoRef
	GaggleBacklog                apiv1.BacklogRef
	AdditionalRepos              []apiv1.RepoRef
	HarnessInfo                  harnessPreflightInfo
	CredentialStores             credentials.StoreResolver
	SandboxPosture               instance.SandboxPosture
	SelfIdentity                 string
	RequireLabelsDefault         string
	BacklogLabelsDefault         string
	BacklogLabelPredicateDefault string
	OwnershipAssigneesDefault    string
	OwnershipUnassignedDefault   string
	Generations                  []string
}
