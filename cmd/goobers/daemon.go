package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

const legacyRuntimeMigrationNote = "legacy flat runtime migrated to per-gaggle layout"

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
	Runner       *runner.Runner
	Runners      map[string]*runner.Runner
	LegacyRunner *runner.Runner
	Telemetry    *telemetry.Client
	RollupDB     *rollup.DB
	// ReadModel is the portal run read model (read.db). Present but unread at
	// this stage — see the construction site and design §6.6 step 1.
	ReadModel *readmodel.Store
	// Watermarks is the source-watermark store (#1922). Separate from ReadModel
	// because they are different databases with different writers: anything that
	// advances a run records here, while only the projector touches ReadModel.
	Watermarks *intake.Store
	// StopProjector shuts the projector's commit loop down. Held on the setup so
	// the daemon's shutdown path stops it with everything else, rather than the
	// loop outliving the process's other goroutines.
	StopProjector func()
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
	// SecretStores resolves store-backed token refs (#683). Built once per
	// setup from cfg.SecretStores so every consumer shares one TTL cache;
	// never nil — an instance with no declared stores gets a registry that
	// fails every store ref closed.
	SecretStores *secretstore.Registry
}

type schedulerDefinitions struct {
	Set               *instance.ConfigSet
	Validation        *validate.Report
	HarnessPreflight  harnessPreflightInfo
	Runner            *runner.Runner
	Runners           map[string]*runner.Runner
	Entries           []localscheduler.WorkflowEntry
	Machines          map[localscheduler.WorkflowIdentity]*workflow.Machine
	GooberDigests     map[localscheduler.WorkflowIdentity]string
	Goobers           map[string]apiv1.GooberSpec
	RepoRefs          map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	OpenPRRefresher   *localscheduler.OpenPRRefresherSet
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
	// RRQ-1/#1101, revised for fleets (#2860): a gaggle/stage requiring a runner
	// capability nothing claims is REPORTED at startup, not fatal.
	//
	// It used to return an error and kill the daemon. That was defensible when a
	// runner was a single process whose capabilities could not change while it
	// ran. It stopped being defensible once stages can be placed on OTHER
	// workers: the daemon is then admitting on behalf of a fleet it cannot
	// enumerate, and "no runner claims os=windows" may simply mean the Windows
	// worker has not started yet. The sibling provider-capability check below
	// says as much in its own comment — a missing runner capability CAN
	// self-heal at runtime, which is exactly why it must not be terminal.
	//
	// Nothing is lost by downgrading it. localscheduler already enforces the
	// same invariant per entry at dispatch (scheduler.go, ReasonMissingCapability):
	// the run is refused, journalled as tick.skipped with the missing capability
	// named, and marked Blocked in telemetry. That path is per-run, self-healing,
	// and describes itself as the seam a multi-runner router grows from. The
	// startup check was an eager, whole-instance, fatal copy of it.
	//
	// So: one unsatisfiable stage no longer takes the whole instance down, and
	// every OTHER gaggle keeps running.
	if err := instance.CheckCapabilityRequirements(cfg.Runner.Capabilities, set); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; affected runs are refused at schedule time with the capability named, other gaggles are unaffected\n", err)
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
	terminalNotifier, err := buildTerminalNotifier(ctx, l, cfg, sharedScrubber, options)
	if err != nil {
		return nil, err
	}

	var tel *telemetry.Client
	var rollupDB *rollup.DB
	var readModel *readmodel.Store
	var readModelEpoch string
	var watermarks *intake.Store
	var stopProjector func()
	var instanceLog *journal.InstanceLog
	defer func() {
		if err == nil {
			return
		}
		if tel != nil {
			_ = tel.Shutdown(context.Background())
		}
		if rollupDB != nil {
			_ = rollupDB.Close()
		}
		// Same order as schedulerSetup.Shutdown: stop the projector before
		// closing the store its commit loop writes through, then release the
		// read model and intake stores. A setup failure between these opening
		// (line ~291 onward) and this function's success return used to leak
		// both handles — invisible on POSIX (an unlinked-but-open file is
		// fine), but on Windows the open handle keeps the underlying temp
		// dir's *.db files locked, so a caller's t.TempDir() cleanup fails
		// outright with "The process cannot access the file because it is
		// being used by another process."
		if stopProjector != nil {
			stopProjector()
		}
		if readModel != nil {
			_ = readModel.Close()
		}
		if watermarks != nil {
			_ = watermarks.Close()
		}
		if instanceLog != nil {
			_ = instanceLog.Close()
		}
	}()
	if cfg.TelemetryEnabled() {
		reportStartupProgress(options.startupProgress, "opening telemetry state")
		var otlpConfig instance.OTLPConfig
		if cfg.Telemetry.OTLP != nil {
			otlpConfig = *cfg.Telemetry.OTLP
		}
		tel, err = buildTelemetryClient(ctx, l, sharedScrubber, sharedReg, otlpConfig, secretStores)
		if err != nil {
			return nil, err
		}
		rollupDB, err = rollup.Open(l.TelemetryDB())
		if err != nil {
			return nil, err
		}
	}

	// Construct read.db alongside the existing store (design §6.6 step 1).
	// Nothing reads it yet: the transition is deliberately additive so that
	// rollback at this stage is deleting a file. The projector, the change
	// feed, and the cutover flag land in later slices.
	//
	// A failure here does NOT fail daemon start. The read model is derived and
	// not yet load-bearing, so refusing to start over a store nothing reads
	// would be an outage caused by an optimization.
	// A failure here must NOT fail daemon start. Nothing reads read.db yet
	// (§6.6 step 1), so refusing to start over a store no request touches
	// would be an outage caused by an optimization — and the daemon's own
	// tests caught exactly that when an earlier version returned an error.
	//
	// The state read uses Background rather than the setup context for the
	// same reason Open's migration does: it is a startup smoke check, not
	// request-scoped work, and a caller whose context is already done must
	// not turn "the store is fine" into "the daemon cannot start".
	// Discard any half-built epoch left by a rebuild that was killed
	// mid-flight (#1925, §6.5). The change-retention pin must release on
	// EVERY terminal outcome — success, abort, discard, and an orphan found
	// at startup. Without this last case an interrupted rebuild blocks change
	// pruning indefinitely and the feed grows without bound for a reason
	// nobody is looking at.
	//
	// Deliberately NOT gated on cfg.TelemetryEnabled() (#2036): read.db answers
	// the portal's run listing, a core feature independent of telemetry, so
	// telemetry.enabled: false must not silently disable it too. Only the
	// measurement population filters below need rollupDB, and that dependency
	// is already an explicit nil check, not this block's gate.
	if discarded, discardErr := readmodel.DiscardStaleRebuilds(filepath.Dir(l.ReadDB())); discardErr != nil {
		fmt.Fprintf(os.Stderr, "warning: discard stale read-model rebuilds: %v\n", discardErr)
	} else if discarded > 0 {
		fmt.Fprintf(os.Stderr, "discarded %d orphaned read-model rebuild(s)\n", discarded)
	}
	reportStartupProgress(options.startupProgress, "opening read-model state")
	if readStore, readErr := readmodel.Open(l.ReadDB()); readErr != nil {
		fmt.Fprintf(os.Stderr, "warning: open read model: %v\n", readErr)
	} else if state, stateErr := readStore.State(context.Background()); stateErr != nil {
		// Reading the state back turns "the file opened" into "the schema is
		// at the version this build expects and the store has an epoch",
		// which is the difference between finding a broken store now and
		// finding it on the first read after the projector lands.
		fmt.Fprintf(os.Stderr, "warning: read model state: %v\n", stateErr)
		_ = readStore.Close()
	} else {
		// Measurement flags (#1782). The four population filters are derived
		// from the telemetry rollup, which no journal event carries, so the
		// read model needs a source for them or `population=` can only ever
		// match nothing.
		//
		// Attached before the first projection rather than after: a run
		// projected without a source has its flags cleared, and nothing later
		// re-projects it. A nil rollupDB detaches, which is correct for a
		// telemetry-disabled instance -- there, the population filters have no
		// data to be right about.
		if rollupDB != nil {
			readStore.WithMeasurement(readservice.NewTelemetryMeasurement(rollupDB))
		}
		// §6.6 step 2: build it by rebuild-from-journals on first start.
		// A completed store is kept and updated incrementally by the writer
		// seam; an unready store is rebuilt synchronously before attachment.
		if err := buildReadModelIfNeeded(ctx, readStore, state, l); err != nil {
			fmt.Fprintf(os.Stderr, "warning: build read model: %v\n", err)
			_ = readStore.Close()
		} else {
			readModelEpoch = state.Epoch
			readModel = readStore

			// The projector (#1923). Opening intake and starting the commit loop
			// is what inverts the coupling: from here the writer records a
			// watermark and forgets, and this owns discovery and application.
			if intakeStore, intakeErr := intake.Open(l.IntakeDB()); intakeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: open intake store: %v\n", intakeErr)
			} else {
				watermarks = intakeStore
				stopProjector = startProjector(ctx, readStore, intakeStore, l, cfg)
			}
		}
	}

	instanceLog, _, err = journal.OpenInstanceLog(l.SchedulerDir(), journal.WithScrubber(sharedScrubber))
	if err != nil {
		return nil, fmt.Errorf("open instance log: %w", err)
	}
	if err := journalLegacyRuntimeMigration(l, instanceLog, runtimeMigration); err != nil {
		return nil, fmt.Errorf("journal legacy runtime migration: %w", err)
	}
	var recoveredClaims []localscheduler.ClaimEntry
	reportStartupProgress(options.startupProgress, "recovering scheduler claims")
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
		return nil, err
	}

	// #712: shared with the Scheduler via SchedulerOptions below — see
	// schedulerSetup.ProviderQuota's doc comment for why a shared pointer,
	// not a Scheduler-owned field, is needed here.
	providerQuota := localscheduler.NewProviderQuotaState()
	runnerRegistry := newDaemonRunnerRegistry()
	definitions, err := buildSchedulerDefinitions(l, cfg, set, report, wg, runnerRegistry, tel, rollupDB, watermarks, instanceLog, sharedReg, nil, providerQuota, terminalNotifier, secretStores, options.startupProgress)
	if err != nil {
		return nil, err
	}
	runnerRegistry.Replace(definitions.Runners)
	reportStartupProgress(options.startupProgress, "initializing retained legacy runtime")
	legacyRunner, legacyWorktrees, err := buildRetainedLegacyRunner(
		l, cfg, set, definitions.Goobers, tel, instanceLog, sharedReg, providerQuota, watermarks, terminalNotifier, definitions.HarnessPreflight, secretStores,
	)
	if err != nil {
		return nil, err
	}
	interventionRegistry := newInterventionDefinitionRegistry(interventionDefinitions(definitions, legacyRunner))
	stableDigest, err := configDirectoryDigest(l.ConfigDir())
	if err != nil || stableDigest != configDigest {
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
		ReadModel:         readModel,
		Watermarks:        watermarks,
		StopProjector:     stopProjector,
		ReadModelEpoch:    readModelEpoch,
		Config:            cfg,
		Definitions:       definitions.Set,
		Worktrees:         definitions.Worktrees,
		WorktreesByGaggle: definitions.WorktreesByGaggle,
		LegacyWorktrees:   legacyWorktrees,
		InstanceLog:       instanceLog,
		Entries:           definitions.Entries,
		Machines:          definitions.Machines,
		GooberDigests:     definitions.GooberDigests,
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
		Interventions:     interventionRegistry,
		SecretStores:      secretStores,
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
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	if other, exists := claims[key]; exists && other.gaggle != gaggle && (alternate || other.alternate) {
		return fmt.Errorf("workcopies path collision: gaggles %s and %s resolve to %s", other.gaggle, gaggle, path)
	}
	claims[key] = workcopyRootClaim{gaggle: gaggle, alternate: alternate}
	return nil
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
	watermarks *intake.Store,
	instanceLog *journal.InstanceLog,
	sharedReg *journal.RegistryScrubber,
	wtManagers map[string]*worktree.Manager,
	providerQuota *localscheduler.ProviderQuotaState,
	terminalNotifier runner.TerminalNotifier,
	stores credentials.StoreResolver,
	startupProgress func(string),
) (*schedulerDefinitions, error) {
	instance.ApplyGaggleOutboxMirror(set)
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(l.ConfigDir(), goobers)
	if err != nil {
		return nil, err
	}
	machines, gooberDigests, resolvedGoobers, harnessWarnings, err := compiledMachinesWithGooberDigestsAndWarnings(
		l.ConfigDir(), set, goobers, instructions, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand,
		true,
	)
	if err != nil {
		return nil, err
	}
	if _, err := appendGooberHarnessWarnings(report, harnessWarnings); err != nil {
		return nil, fmt.Errorf("append harness validation warnings: %w", err)
	}
	harnessInfo, err := preflightHarnesses(goobers, set.Workflows, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand)
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
	selfIdentities := selfIdentitiesByGaggle(cfg, set)
	requireLabelsDefaults := requireLabelsByGaggle(set)
	// Each gaggle's project repo drives its runner's per-gaggle credential
	// scoping (MGV-5, #1012): its stages are granted that repo's own token. A
	// gaggle with no configured Gaggle object (a single-gaggle default) has no
	// entry here, so its runner falls back to the first repo's token unchanged.
	gaggleProjects := make(map[string]apiv1.RepoRef, len(set.Gaggles))
	gaggleAdditionalRepos := make(map[string][]apiv1.RepoRef, len(set.Gaggles))
	workcopyLayouts := make(map[string]instance.Layout, len(set.Gaggles))
	workcopyRoots := make(map[string]workcopyRootClaim, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggle := &set.Gaggles[i]
		gaggleProjects[gaggle.Name] = gaggle.Spec.Project
		gaggleAdditionalRepos[gaggle.Name] = gaggle.Spec.AdditionalRepos
		scoped, layoutErr := instance.EffectiveWorkcopiesLayout(l.ForGaggle(gaggle.Name), cfg, gaggle)
		if layoutErr != nil {
			return nil, fmt.Errorf("gaggle %s: %w", gaggle.Name, layoutErr)
		}
		managerRoot := scoped.WorkcopiesDir()
		if configuredProject, ok := configuredRepoForProject(cfg, gaggle.Spec.Project); ok && configuredProject.Pinned() {
			managerRoot = scoped.WorkcopiesBaseDir()
		}
		alternateRoot := cfg.Workcopies != nil && cfg.Workcopies.Root != ""
		if gaggle.Spec.Workcopies != nil && gaggle.Spec.Workcopies.Root != "" {
			alternateRoot = true
		}
		if err := claimWorkcopyRoot(workcopyRoots, gaggle.Name, managerRoot, alternateRoot); err != nil {
			return nil, err
		}
		workcopyLayouts[gaggle.Name] = scoped
	}
	sandboxPostures := sandboxPosturesByGaggle(cfg, set)
	runners := make(map[string]*runner.Runner)
	for _, gaggle := range configuredGaggleNames(set) {
		reportStartupProgress(startupProgress, fmt.Sprintf("initializing gaggle %q runtime", gaggle))
		scoped := workcopyLayouts[gaggle]
		rn, manager, err := buildRuntimeRunner(
			scoped, cfg, resolvedGoobers, instructions, tel, instanceLog, sharedReg, wtManagers[gaggle],
			providerQuota, watermarks, terminalNotifier, branchNamespaces, gaggleProjects[gaggle], gaggleAdditionalRepos[gaggle], harnessInfo,
			stores, sandboxPostures[gaggle], selfIdentities[gaggle], requireLabelsDefaults[gaggle],
		)
		if err != nil {
			return nil, fmt.Errorf("initialize gaggle %q runtime: %w", gaggle, err)
		}
		wtManagers[gaggle] = manager
		runners[gaggle] = rn
		reportStartupProgress(startupProgress, fmt.Sprintf("gaggle %q runtime ready", gaggle))
	}

	openPRRefresher, err := buildOpenPRRefresher(cfg, set.Workflows, gaggleProjects, sharedReg, branchNamespaces, l.SchedulerDir(), stores)
	if err != nil {
		return nil, err
	}
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	credResolver, _, err := buildCredentials(cfg, stores, "", "", nil, sharedReg)
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
		var scheduleBackoffs []localscheduler.IdleBackoffConfig
		var sigs []string
		hasRepositoryWebhook := false
		var pollPriority int32
		pollPrioritySet := false
		for _, trigger := range wf.Spec.Triggers {
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
				for _, event := range trigger.Events {
					sigs = append(sigs, webhookhttp.SignalName(event))
				}
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
			if err := validateScheduledWorkflowCredentialEnvironment(machine, cfg, project); err != nil {
				return nil, err
			}
		}
		pollFallbackCause := ""
		if hasRepositoryWebhook && len(scheds) > 0 {
			switch {
			case repoRefs[identity].Provider != apiv1.ProviderGitHub:
				pollFallbackCause = "repository provider does not support GitHub webhook delivery"
			case !cfg.WebhookSecretConfigured():
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
		requiredCaps := instance.WorkflowRequiredCapabilities(gagglesByName[wf.Spec.Gaggle], *wf)
		instanceControls := cfg.RunConditions.RunControls()
		if repo, ok := configuredRepoForProject(cfg, repoRefs[identity]); ok {
			instanceControls = repo.EffectiveRunControls(instanceControls)
		}
		controls, err := runcontrol.Resolve(
			instanceControls,
			gagglesByName[wf.Spec.Gaggle].Spec.RunControls,
			wf.Spec.RunControls,
		)
		if err != nil {
			return nil, fmt.Errorf("workflow %q run controls: %w", wf.Name, err)
		}
		backlogCounter, err := buildBacklogCounter(cfg, gagglesByName[wf.Spec.Gaggle], wf, repoRefs[identity], credResolver, sharedReg, l.SchedulerDir(), providerQuota, l.Root)
		if err != nil {
			return nil, err
		}
		entries = append(entries, localscheduler.WorkflowEntry{
			Workflow:          wf.Name,
			WorkflowVersion:   machine.Def.Version,
			WorkflowDigest:    machine.Digest(),
			Gaggle:            wf.Spec.Gaggle,
			Readiness:         wf.Spec.Readiness,
			Schedules:         scheds,
			ScheduleBackoffs:  scheduleBackoffs,
			Signals:           sigs,
			PollFallbackCause: pollFallbackCause,
			BacklogCounter:    backlogCounter,
			ScheduleDemandCounter: buildScheduleDemandCounter(
				cfg, wf, repoRefs[identity], credResolver, sharedReg, l.SchedulerDir(),
				branchNamespaces[wf.Spec.Gaggle], providerQuota,
			),
			// The current provider-backed demand counters use GitHub; charge the
			// provider actually called rather than a future configured adapter.
			PollProvider: apiv1.ProviderGitHub,
			PollPriority: pollPriority,
			Starter:      &trackedStarter{r: runners[wf.Spec.Gaggle], machine: machine, runControls: controls.Overrides(), requiredCaps: requiredCaps, wg: wg, l: l.ForGaggle(wf.Spec.Gaggle), tel: tel, rollupDB: rollupDB, watermarks: watermarks, log: instanceLog, runners: runnerRegistry},
			RepoRef:      repoRefs[identity],
			// RRQ-1/#1101 schedule-match + #735 host preflight both consume this.
			RequiredCapabilities: requiredCaps,
		})
		entries[len(entries)-1].GooberDigest = gooberDigests[identity]
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
		GooberDigests:     gooberDigests,
		Goobers:           resolvedGoobers,
		RepoRefs:          repoRefs,
		OpenPRRefresher:   openPRRefresher,
		Worktrees:         firstWorktrees,
		WorktreesByGaggle: wtManagers,
	}, nil
}

func validateScheduledWorkflowCredentialEnvironment(machine *workflow.Machine, cfg *instance.Config, project apiv1.RepoRef) error {
	envByCapability, err := scheduledWorkflowCredentialEnvironments(cfg, project)
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

func scheduledWorkflowCredentialEnvironments(cfg *instance.Config, project apiv1.RepoRef) (map[string]string, error) {
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

	overrides := make([]credentials.Grant, 0, len(daemonIdentityCapabilities)+len(cfg.Credentials))
	if cfg.DaemonIdentity != nil {
		for _, capability := range daemonIdentityCapabilities {
			overrides = append(overrides, credentials.Grant{Capability: string(capability), Ref: daemonIdentityRefName})
		}
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
	caps := make([]string, len(credentialedCapabilities))
	for i, capability := range credentialedCapabilities {
		caps[i] = string(capability)
	}
	grants := credentials.RunnerGrants(bindings, owner, project.Name, caps, overrides)
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

func buildRetainedLegacyRunner(
	l instance.Layout,
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	tel *telemetry.Client,
	instanceLog *journal.InstanceLog,
	sharedReg *journal.RegistryScrubber,
	providerQuota *localscheduler.ProviderQuotaState,
	watermarks *intake.Store,
	terminalNotifier runner.TerminalNotifier,
	harnessInfo harnessPreflightInfo,
	stores credentials.StoreResolver,
) (*runner.Runner, *worktree.Manager, error) {
	retained, err := retainedLegacyRuntimeExists(l)
	if err != nil || !retained {
		return nil, nil, err
	}
	// Legacy retained runtime: no per-gaggle project scoping — a zero project
	// repo leaves credentials on the first-repo default (unchanged behavior).
	instructions, err := loadGooberInstructions(l.ConfigDir(), goobers)
	if err != nil {
		return nil, nil, err
	}
	return buildRuntimeRunner(
		l, cfg, goobers, instructions, tel, instanceLog, sharedReg, nil, providerQuota,
		watermarks, terminalNotifier, branchNamespacesByGaggle(set), apiv1.RepoRef{}, nil, harnessInfo, stores,
		// Legacy retained runtime is not gaggle-scoped, so only the
		// instance-wide posture can apply (no gaggle override to consult).
		instance.EffectiveAgenticSandbox(cfg, nil),
		instance.EffectiveSelfIdentity(cfg, nil),
		// Same reasoning: no gaggle to consult for a RequireLabels default.
		"",
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
	instructions map[string]string,
	tel *telemetry.Client,
	instanceLog *journal.InstanceLog,
	sharedReg *journal.RegistryScrubber,
	manager *worktree.Manager,
	providerQuota *localscheduler.ProviderQuotaState,
	watermarks *intake.Store,
	terminalNotifier runner.TerminalNotifier,
	branchNamespaces map[string]string,
	gaggleProject apiv1.RepoRef,
	additionalRepos []apiv1.RepoRef,
	harnessInfo harnessPreflightInfo,
	stores credentials.StoreResolver,
	sandboxPosture instance.SandboxPosture,
	selfIdentity string,
	requireLabelsDefault string,
) (*runner.Runner, *worktree.Manager, error) {
	runnerCfg, manager, err := buildRunnerConfig(runnerCompositionInput{
		Layout:               l,
		Config:               cfg,
		Goobers:              goobers,
		InstructionsByGoober: instructions,
		Telemetry:            tel,
		SharedRegistry:       sharedReg,
		WorktreeManager:      manager,
		BranchNamespaces:     branchNamespaces,
		GaggleProject:        gaggleProject,
		AdditionalRepos:      additionalRepos,
		HarnessInfo:          harnessInfo,
		CredentialStores:     stores,
		SandboxPosture:       sandboxPosture,
		ProviderQuota:        providerQuota,
	})
	if err != nil {
		return nil, nil, err
	}
	runnerCfg.BacklogQueryAssignedTo = selfIdentity
	runnerCfg.BacklogQueryRequireLabels = requireLabelsDefault
	runnerCfg.JournalAdvanced = runIntakeObserver(watermarks, instanceLog)
	runnerCfg.PrepareTerminal, err = buildTerminalBranchPreparer(l, cfg, gaggleProject, sharedReg, stores)
	if err != nil {
		return nil, nil, err
	}
	runnerCfg.FinalizeTerminal = func(runID string, _ journal.RunPhase) error {
		return finalizeTerminalRun(l, instanceLog, manager, runID)
	}
	runnerCfg.RateLimited = buildRateLimitedHandler(providerQuota)
	if terminalNotifier != nil {
		circuitBreaker := runnerCfg.NotifyTerminal
		runnerCfg.NotifyTerminal = func(runID string, phase journal.RunPhase, finalState string) error {
			if circuitBreaker != nil {
				_ = circuitBreaker(runID, phase, finalState)
			}
			return terminalNotifier(runID, phase, finalState)
		}
	}
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
	if err := s.RollupDB.IngestSchedulerLog(context.Background(), s.InstanceLog.Dir()); err != nil {
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
	// The projector stops BEFORE its store closes. Its commit loop holds the
	// only writable handle, so closing read.db underneath a commit in flight
	// would fail that projection — and the failure would look like corruption
	// rather than shutdown. Stop is synchronous: it waits for the loop to drain.
	if s.StopProjector != nil {
		s.StopProjector()
	}
	if s.ReadModel != nil {
		_ = s.ReadModel.Close()
	}
	if s.Watermarks != nil {
		_ = s.Watermarks.Close()
	}
	if s.InstanceLog != nil {
		_ = s.InstanceLog.Close()
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
	runControls  apiv1.RunControls
	requiredCaps []string
	wg           *sync.WaitGroup
	l            instance.Layout
	tel          *telemetry.Client
	rollupDB     *rollup.DB
	watermarks   *intake.Store
	log          *journal.InstanceLog
	runners      *daemonRunnerRegistry
}

func (s *trackedStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.wg.Add(1)
	defer s.wg.Done()
	untrack := s.runners.Track(req.RunID, s.machine.Def.Name, s.r)
	defer untrack()
	res, err := s.r.Start(ctx, runner.StartInput{
		RunID:                req.RunID,
		Machine:              s.machine,
		GooberDigest:         req.GooberDigest,
		Gaggle:               req.Gaggle,
		Trigger:              req.Trigger,
		RepoRef:              req.RepoRef,
		Item:                 req.Item,
		RunControls:          s.runControls,
		RequiredCapabilities: s.requiredCaps,
	})
	ingestRunTelemetry(s.tel, s.rollupDB, s.watermarks, s.l, req.RunID, s.log)
	return localscheduler.StartResult{
		Phase:          res.Phase,
		FinalState:     res.FinalState,
		NoWork:         res.NoWork,
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
func resumeInterruptedRuns(ctx context.Context, l instance.Layout, rn *runner.Runner, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, gooberDigests map[localscheduler.WorkflowIdentity]string, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, watermarks *intake.Store, release func(runID, workflow string), wg *sync.WaitGroup) (resumed []string, warned []string, err error) {
	return resumeInterruptedRunsWithRunners(ctx, l, nil, rn, nil, machines, gooberDigests, repoRefs, log, tel, rollupDB, watermarks, release, wg)
}

func interruptedRunMachine(id journal.RunIdentity, current *workflow.Machine) (*workflow.Machine, string) {
	if id.WorkflowDigest != "" && current.Digest() != id.WorkflowDigest {
		return nil, "pinned-snapshot"
	}
	return current, "current-config"
}

func resumeInterruptedRunsWithRunners(ctx context.Context, l instance.Layout, runners map[string]*runner.Runner, fallback *runner.Runner, runnerRegistry *daemonRunnerRegistry, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, gooberDigests map[localscheduler.WorkflowIdentity]string, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, log *journal.InstanceLog, tel *telemetry.Client, rollupDB *rollup.DB, watermarks *intake.Store, release func(runID, workflow string), wg *sync.WaitGroup) (resumed []string, warned []string, err error) {
	runDirs, err := l.RunDirs()
	if err != nil {
		return nil, nil, err
	}
	for _, runsDir := range runDirs {
		entries, exists, err := readDirectory(runsDir)
		if !exists {
			continue
		}
		if err != nil {
			return resumed, warned, fmt.Errorf("read runs directory: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(runsDir, e.Name())
			rd, err := journal.OpenRead(dir)
			if err != nil {
				if errors.Is(err, journal.ErrNotRunDirectory) {
					continue
				}
				return resumed, warned, fmt.Errorf("open run journal %q: %w", e.Name(), err)
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
					// #2190: a run that resumed here and was already terminal
					// took a different path than a normal terminal run's
					// ingestRunTelemetry call below (line ~1080) — it never
					// recorded its intake watermark, so the read model never
					// discovered it advanced.
					recordRunIntake(watermarks, runLayout, id.RunID, log)
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
			// Never reinterpret a historical run under the current workflow
			// merely because the name still matches.
			machine, machineSource := interruptedRunMachine(id, machine)
			repoRef := repoRefs[identity]
			gooberDigest := gooberDigests[identity]

			resumed = append(resumed, id.RunID)
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
					return resumed, warned, fmt.Errorf("journal recovery for run %q: %w", id.RunID, err)
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
				ingestRunTelemetry(tel, rollupDB, watermarks, runLayout, runID, log)
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
			}(id.RunID, id.Gaggle, id.Workflow, gooberDigest, rn, runLayout, untrack)
		}
	}
	return resumed, warned, nil
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
	roots, err := l.RunDirs()
	if err != nil {
		return err
	}
	if _, err := store.BuildFromJournals(ctx, roots); err != nil {
		return err
	}
	return store.MarkReady(ctx)
}
