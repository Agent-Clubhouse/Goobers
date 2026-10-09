package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/apireadcache"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/configauthoring"
	"github.com/goobers/goobers/internal/daemonheartbeat"
	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/ephemeraltmp"
	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/oidcauth"
	"github.com/goobers/goobers/internal/platform/memstat"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/sweepreport"
	"github.com/goobers/goobers/internal/telemetry"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/version"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/winsvc"
	"github.com/goobers/goobers/internal/worktree"
)

// drainProgressInterval controls shutdown progress reporting. It is a variable
// so tests can exercise cadence without waiting for the production interval.
var drainProgressInterval = 10 * time.Second

// claimRecoverInterval bounds how often runUpContext sweeps the claim ledger
// for expired leases while running, catching a live run that overran its
// lease without crashing (localscheduler.ClaimLedger.RecoverExpired's doc:
// "call once at startup... and periodically thereafter"). Var, not const, so
// tests can shrink it rather than waiting out a real 5 minutes.
var claimRecoverInterval = 5 * time.Minute

// stalledRunSweepInterval bounds how quickly the daemon notices a run that has
// crossed its configured journal-silence deadline.
var stalledRunSweepInterval = time.Minute

// worktreeRetentionSweepInterval bounds how often a running daemon re-sweeps
// crash-orphaned worktrees (Manager.Reap) and configured retention
// (pruneConfiguredRetention). Both previously ran only once at startup
// (#2052): a daemon that stayed up for weeks accumulated kept failure
// worktrees and never reclaimed the disk until its next restart. Var, not
// const, so tests can shrink it rather than waiting out a real 6 hours.
var worktreeRetentionSweepInterval = 6 * time.Hour

// mergedPRCostSweepInterval is declared in daemoncostreconcile.go. It is
// intentionally separate from claim recovery and retention: provider reads
// and comment writes must never delay correctness-critical claim cleanup.

// delegationSweepInterval bounds how often runUpContext checks for delegated
// trigger requests (#343, rundelegate.go) from a `goobers run` invocation
// that found this daemon already holding up.lock. Deliberately much shorter
// than claimRecoverInterval — a human waiting on `goobers run` to return
// expects it to feel responsive, not lag behind a background maintenance
// cadence. Var, not const, so tests can shrink it further.
var delegationSweepInterval = 2 * time.Second

// heartbeatInterval is a var so daemon tests do not wait a full minute.
var heartbeatInterval = time.Minute

// apiReadCacheLockSweepInterval bounds how often a running daemon re-sweeps
// stale api-read-cache per-list-key lock files (apireadcache.CleanStaleLocks).
// Well under the cache's 24h stale-lock age so a lock crosses the staleness
// cutoff and gets reclaimed within one interval of becoming eligible, rather
// than waiting on incidental re-construction of the cache from an unrelated
// poller (#4251). Var, not const, so tests can shrink
// it rather than waiting out a real hour.
var apiReadCacheLockSweepInterval = time.Hour

const sweepErrorReportEvery = 12

var httpShutdownGrace = 5 * time.Second

// shutdownHTTPServers shuts the API listener down and, if one is running,
// the webhook listener, each against its own fresh grace-period context
// (#4571). Before this both calls shared a single context: the first
// Shutdown could consume the entire deadline, leaving the second with none
// and making it return context deadline exceeded immediately — turning an
// otherwise-successful drain into exit status 1. webhookServer may be nil,
// in which case webhookErr is always nil.
func shutdownHTTPServers(apiServer, webhookServer *httpapi.Server, grace time.Duration) (apiErr, webhookErr error) {
	apiCtx, apiCancel := context.WithTimeout(context.Background(), grace)
	apiErr = apiServer.Shutdown(apiCtx)
	apiCancel()
	if webhookServer != nil {
		webhookCtx, webhookCancel := context.WithTimeout(context.Background(), grace)
		webhookErr = webhookServer.Shutdown(webhookCtx)
		webhookCancel()
	}
	return apiErr, webhookErr
}

const daemonAPIAddressFileName = daemonstate.APIAddressFileName

func daemonDiscoveryIdentity(root string) httpapi.DiscoveryIdentity {
	build := version.Get()
	return httpapi.DiscoveryIdentity{
		DaemonInstanceID: readservice.InspectRootIdentity(root).ID,
		Build: readservice.BuildMetadata{
			Version: build.Version,
			Commit:  build.Commit,
			Date:    build.Date,
		},
	}
}

func daemonReadHandlerOptions(root string, setup *schedulerSetup) []httpapi.HandlerOption {
	options := []httpapi.HandlerOption{
		httpapi.WithDiscoveryIdentity(daemonDiscoveryIdentity(root)),
		httpapi.WithTelemetryReadAvailability(setup.RollupDB != nil),
	}
	if setup.ReadModel != nil {
		options = append(options, httpapi.WithChangeFeedStream(setup.ReadModel))
	}
	return options
}

func newConfigAuthoringReader(ctx context.Context, layout instance.Layout, config *instance.Config) (configauthoring.Reader, error) {
	root := layout.ConfigDir()
	kind := apicontract.ConfigSourceLocal
	writable := true
	if config != nil && config.WorkflowSource != nil {
		switch config.WorkflowSource.Kind {
		case instance.WorkflowSourceKindLocalDir:
			var err error
			root, err = (instance.LocalDirSource{Path: config.WorkflowSource.Path}).Resolve(ctx)
			if err != nil {
				return nil, fmt.Errorf("resolve local configuration source: %w", err)
			}
		case instance.WorkflowSourceKindGit:
			kind = apicontract.ConfigSourceGit
			writable = false
		}
	}
	return configauthoring.NewReader(root, kind, writable)
}

func appendWorkerDivergenceHandlerOption(options []httpapi.HandlerOption, setup *schedulerSetup) ([]httpapi.HandlerOption, error) {
	option, err := newWorkerDivergenceHandlerOption(setup.InstanceLog, setup.Config)
	if err != nil {
		return options, err
	}
	return append(options, option), nil
}

func reportDaemonStartupError(stderr io.Writer, operation string, err error) int {
	pf(stderr, "error: %s: %v\n", operation, err)
	return 1
}

// diagnosticsMode is set true by `goobers up --diagnostics`. Read in
// buildRunnerConfig to arm the executor's per-stage diagnostics watchdog and
// un-truncate stage output. A package var (like runProcessExits) so it threads
// to the runner wiring without changing buildSchedulerSetup's signature across
// its many test callers; default false keeps every test and a normal daemon on
// the zero-cost path.
var diagnosticsMode bool

// diagnosticsMaxOutputBytes is the per-stream stage output cap under
// --diagnostics — large enough that a full goroutine dump or a verbose hung
// stage's output is never clipped by the default 1 MiB cap.
const diagnosticsMaxOutputBytes int64 = 64 << 20 // 64 MiB

// apiListenAddress resolves the daemon's HTTP listen address from config. It is
// a package var solely so the cmd/goobers test suite can force an ephemeral
// loopback port (127.0.0.1:0) in place of the fixed default, keeping every
// daemon-lifecycle test hermetic against a co-located daemon already holding
// the default port (#798 — the self-host instance's own `goobers up` daemon).
// Production leaves it at this identity default, so the configured address is
// used verbatim; see testmain_test.go for the test-suite redirect.
var apiListenAddress = func(c *instance.Config) string { return c.APIListenAddress() }

type sweepErrorReporter struct{ sweepreport.Reporter }

func newSweepErrorReporter(log *journal.InstanceLog, code string) *sweepErrorReporter {
	return &sweepErrorReporter{sweepreport.New(log, code, sweepErrorReportEvery)}
}

// sweepOrphanedEphemeralTmp reclaims any `goobers-ephemeral-tmp-*` directory
// an OOMKill orphaned on a prior generation of this process (#3969) — an
// OOMKill takes pid 1 with no unwinding, so the deferred Reclaim call that
// normally cleans one up never runs. Called once, early in daemon startup
// before this process establishes a Scope of its own, so every such
// directory already in the temp root belongs to a prior generation, never a
// live one (see ephemeraltmp.SweepOrphans' own doc for why that invariant
// does not generalize past this exact call site). Gated on the same
// declaration that governs whether this daemon ever creates one, so a self
// runner that does not declare tmp:ephemeral sees no behavior change here
// either.
func sweepOrphanedEphemeralTmp(cfg *instance.Config, log *journal.InstanceLog) {
	if !cfg.SelfRunnerEnforces(instance.RunnerRestrictionTmpEphemeral) {
		return
	}
	_, sweepErr := ephemeraltmp.SweepOrphans("")
	newSweepErrorReporter(log, "ephemeral_tmp_sweep_failed").report(sweepErr)
}

func startAPIReadCacheLockSweepTicker(ctx context.Context, l instance.Layout) <-chan struct{} {
	return apireadcache.StartLockSweep(ctx, l.SchedulerDir(), apiReadCacheLockSweepInterval)
}

func (r *sweepErrorReporter) report(err error) {
	r.Report(err)
}

func runUp(args []string, stdout, stderr io.Writer) int {
	// When the process was launched by the Windows Service Control Manager, run
	// under the SCM so SERVICE_CONTROL_STOP cancels the daemon context — the
	// same graceful-drain path SIGTERM drives on unix (issue #639). Off Windows
	// IsWindowsService is always false, so the unix signal path below is
	// unchanged.
	if isService, err := winsvc.IsWindowsService(); err == nil && isService {
		code, runErr := winsvc.Run("goobers", func(ctx context.Context) int {
			return runUpContext(ctx, args, stdout, stderr)
		})
		if runErr != nil {
			pf(stderr, "error: run as Windows service: %v\n", runErr)
			return 1
		}
		return code
	}
	ctx, force, stop := signals.SetupSignalContextWithForce()
	defer stop()
	return runUpContextWithForce(ctx, force, args, stdout, stderr)
}

func handleSpansOnlyRunCleanup(l instance.Layout, remove bool, stdout io.Writer) error {
	runDirs, err := l.RunDirs()
	if err != nil {
		return err
	}
	candidates, err := journal.SpansOnlyRunCandidates(runDirs)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		pf(stdout, "spans-only run cleanup candidate: %s\n", candidate)
	}
	if len(candidates) == 0 {
		return nil
	}
	directoryNoun := "directories"
	if len(candidates) == 1 {
		directoryNoun = "directory"
	}
	if !remove {
		pf(stdout, "dry run: %d spans-only run %s preserved; restart with --cleanup-spans-only-runs to delete\n",
			len(candidates), directoryNoun)
		return nil
	}
	removed, err := journal.RemoveSpansOnlyRuns(candidates)
	if err != nil {
		return err
	}
	directoryNoun = "directories"
	if removed == 1 {
		directoryNoun = "directory"
	}
	pf(stdout, "removed %d spans-only run %s\n", removed, directoryNoun)
	return nil
}

const upHelp = "Usage: goobers up [--quiet] [--diagnostics] [--notify[=all]] [--watch-config] [--drain-timeout duration] [--skip-preflight] [--cleanup-spans-only-runs] [--disable-read-model-reads] [path]\n\n" +
	"Run the daemon: the embedded scheduler (cron triggers + run conditions)\n" +
	"plus the local runner, loopback HTTP API, and configured GitHub webhook\n" +
	"listener (default path \".\"). Blocks\n" +
	"until interrupted (SIGINT/SIGTERM), then drains in-flight runs indefinitely\n" +
	"by default. --drain-timeout forces shutdown after a deadline; a repeated\n" +
	"signal always forces shutdown without prompting. Interrupted runs resume\n" +
	"from their last durable checkpoints on the next startup before\n" +
	"exiting. Exit codes: 0 = clean shutdown, 1 = daemon/API failure,\n" +
	"2 = usage/IO error.\n\n" +
	"After readiness and every 15 minutes, the daemon scans a bounded seven-day\n" +
	"window of recently merged GitHub Goobers pull requests and publishes\n" +
	"any missing or updated cost summary. This is a workflow-independent backstop; the\n" +
	"normal post-merge stage still publishes synchronously when Goobers merges.\n\n" +
	"Legacy spans-only run directories are reported as cleanup candidates\n" +
	"and preserved by default. --cleanup-spans-only-runs deletes them at\n" +
	"startup after reporting each candidate.\n\n" +
	"Startup validates the resolved instance config and refuses to run on\n" +
	"errors. --skip-preflight bypasses that refusal with a prominent warning.\n" +
	"It does not skip the harness admission preflight: a workflow whose agentic\n" +
	"stage needs a harness that fails its startup check is still refused, while\n" +
	"other workflows keep running.\n\n" +
	"A Git workflowSource continuously reconciles its tracked ref. Local Git\n" +
	"ref changes wake the loop immediately; periodic fetch-and-compare polling\n" +
	"is always active, and authenticated GitHub push deliveries wake it when\n" +
	"webhook.secret is configured. Invalid revisions are rejected with the\n" +
	"last-known-good definitions left running. Direct edits to the materialized\n" +
	"config directory are watched by default; --watch-config=false explicitly\n" +
	"disables that watcher. instance.yaml is loaded only at daemon startup and\n" +
	"is never hot-reloaded; changes to it, including retention: and\n" +
	"telemetry.retention:, require a daemon restart. Existing runs retain their\n" +
	"pinned definitions.\n\n" +
	"--diagnostics turns on deep, opt-in capture for hard hangs: any\n" +
	"deterministic stage still running past a couple of minutes gets a\n" +
	"periodic native process sample + process tree + open-fd (lsof)\n" +
	"snapshot recorded as a run artifact, and stage stdout/stderr are kept\n" +
	"un-truncated. Verbose and slightly heavier; leave off for normal runs.\n\n" +
	"--disable-read-model-reads is the design's §6.6 read-model rollback: it\n" +
	"forces every list request to scan the authoritative journals for this\n" +
	"run, bypassing both read.db and telemetry.db as run-candidate indexes.\n" +
	"This can be slow on a large history. A flag flip and a restart, not a\n" +
	"deploy — use it if the read-model list path is ever suspected of serving\n" +
	"wrong or incomplete results.\n\n" +
	"These five behavior controls are intentionally flag-only: --watch-config\n" +
	"controls the process-local definition watcher, --diagnostics is temporary\n" +
	"debug capture, --drain-timeout applies only after this process receives a\n" +
	"shutdown signal, --skip-preflight is an unsafe startup escape hatch, and\n" +
	"--disable-read-model-reads is an emergency rollback. Keeping them out of\n" +
	"instance.yaml prevents temporary operational overrides from becoming\n" +
	"durable policy. `goobers status --daemon` reports their effective values.\n"

// runUpContext is runUp's testable core: the OS signal wiring lives only in
// runUp, so tests can drive shutdown deterministically via ctx cancellation
// instead of sending real signals.
func runUpContext(parentCtx context.Context, args []string, stdout, stderr io.Writer) int {
	return runUpContextWithForce(parentCtx, nil, args, stdout, stderr)
}

func daemonTriggerSweep(
	ctx context.Context,
	l instance.Layout,
	log *journal.InstanceLog,
	durableTriggers *durableTriggerService,
	sched *localscheduler.Scheduler,
	heartbeat *atomic.Int64,
	options triggerSweepOptions,
) func() error {
	return func() error {
		var sweepErr error
		if options == (triggerSweepOptions{}) {
			sweepErr = sweepPendingTriggers(ctx, l.SchedulerDir(), log, sched, time.Now)
		} else {
			sweepErr = sweepPendingTriggersWithOptions(ctx, l.SchedulerDir(), log, sched, time.Now, options)
		}
		err := errors.Join(durableTriggers.Drain(ctx), sweepErr)
		return recordTriggerSweepProgress(heartbeat, err, time.Now())
	}
}

func runUpContextWithForce(parentCtx context.Context, force <-chan struct{}, args []string, stdout, stderr io.Writer) int {
	u := &upSession{upEnvironment: upEnvironment{parentCtx: parentCtx, force: force, args: args, stdout: stdout, stderr: stderr}}
	return u.run()
}

// upEnvironment contains command inputs shared by the ordered daemon phases.
type upEnvironment struct {
	args         []string
	ctx          context.Context
	force        <-chan struct{}
	parentCtx    context.Context
	processStart time.Time
	stderr       io.Writer
	stdout       io.Writer
	stopDaemon   func()
	webhookGate  *webhookhttp.DispatchGate
}

// upSession owns phase results for one daemon lifetime. It is never copied:
// readiness atomics, the wait group, and deferred cleanup all refer to this instance.
// Each phase calls the next before returning, so its defers cover all later phases
// and unwind in the original reverse order, including on early startup failures.
type upSession struct {
	upEnvironment
	upOptions
	upReadiness
	upStartup
	upServices
	upRecovery
	upWorkers
	upShutdown
}

type upOptions struct {
	cleanupSpansOnlyRuns  *bool
	diagnostics           *bool
	disableReadModelReads *bool
	drainTimeout          *time.Duration
	notifications         notifyFlag
	quiet                 *bool
	root                  string
	skipPreflight         *bool
	watchConfig           *bool
}

// upReadiness exposes named startup milestones on /readyz (#3806). The phases
// flip these in order; ready remains the shared source of truth for /readyz and
// /api/v1/health. The named milestones add diagnostics without gating work.
type upReadiness struct {
	apiListening            atomic.Bool
	configLoaded            atomic.Bool  // instance config + scheduler wiring validated
	lastTickAtNanos         atomic.Int64 // in-memory heartbeat /healthz reads (#3806); unix nanos
	lastTriggerSweepAtNanos atomic.Int64
	// planeReady (#4252): true once the FULL versioned handler — with the
	// credential/blob/journal/surrender plane routes wired in — has been
	// swapped into the SwitchHandler (apiHandler.Set below), independent
	// of resumeComplete/sweepsStarted/ready — see httpapi.ReadinessStatus's
	// doc comment for the plane-ready/scheduler-ready split this drives.
	// apiListening above flips earlier still (the bare listener, serving
	// only /healthz/readyz/503) and is not sufficient on its own.
	planeReady      atomic.Bool
	ready           atomic.Bool
	resumeComplete  atomic.Bool // crash-resume of interrupted runs finished
	schedulerTicked atomic.Bool // scheduler's heartbeat ticked at least once (liveness grace)
	stateOpen       atomic.Bool // scheduler's run-tracking state reconciled from disk
	sweepsStarted   atomic.Bool // initial sweeps ran once and their periodic tickers are live
}

type upStartup struct {
	apiAddressPath         string
	apiAddressPublished    bool
	apiHandler             *httpapi.SwitchHandler
	apiLog                 *log.Logger
	apiServer              *httpapi.Server
	apiStopped             bool
	claimRecoveryGate      *localscheduler.RecoveryGate
	currentDaemon          *daemonIdentity
	l                      instance.Layout
	livenessTimeout        time.Duration
	lockPath               string
	priorLock              priorDaemonLock
	probes                 *daemonProbeState
	recoveryInventory      *recoveryInventoryGate
	retentionGate          *retentionSweepGate
	setup                  *schedulerSetup
	startTelemetryReplay   func()
	startupConfig          *instance.Config
	storageGate            *localscheduler.StorageGate
	storageThresholds      instance.StorageThresholds
	telemetryRetentionGate *retentionSweepGate
	tracker                *startupPhaseTracker
	webhookServer          *httpapi.Server
	wg                     sync.WaitGroup
}

type upServices struct {
	apiAuthorizer              httpapi.Authorizer
	apiHandlerOpts             []httpapi.HandlerOption
	blobStore                  *blobstore.Dir
	cancelPlane                *daemonCancelService
	configDigests              *configDigestPublisher
	credentialPlane            *daemonCredentialService
	durableTriggers            *durableTriggerService
	engineClient               *daemonEngineClient
	engineGuards               *engineRunGuards
	interventions              *intervention.Service
	liveJournals               *livejournal.Writer
	reads                      *readservice.Local
	recoverExpiredClaims       func(now time.Time) ([]localscheduler.ClaimEntry, error)
	shutdownSetup              func() error
	stopDaemonHealth           func()
	terminalCleanupRetryErrors *sweepErrorReporter
	triggerPlane               *daemonTriggerService
	workflowMutations          *workflowMutationService
	worktreeRetentionErrors    *sweepErrorReporter
}

type upRecovery struct {
	applySweep               func() error
	applySweepErrors         *sweepErrorReporter
	cancelSweep              func() error
	cancelSweepErrors        *sweepErrorReporter
	claimLiveness            localscheduler.RunLivenessProbe
	cleanupRetries           *terminalCleanupRetryRegistry
	migrationBackupGaggles   []string
	openPRs                  *openPRLoop
	recoveryRunDirs          []string
	reloader                 *configReloader
	resumeResult             resumeOutcome
	sched                    *localscheduler.Scheduler
	sourceApplier            *workflowSourceApplier
	sourceReconcileWake      chan struct{}
	stalledSweepErrors       *sweepErrorReporter
	stopClaimAdminSweep      func()
	sweepStalled             func(now time.Time, recoveryRunDirs ...[]string) error
	telemetryRetentionConfig instance.TelemetryRetentionConfig
	triggerSweep             func() error
	triggerSweepErrors       *sweepErrorReporter
}

type upWorkers struct {
	apiReadCacheLockSweepTickerDone    <-chan struct{}
	applyTickerDone                    chan struct{}
	cancelTickerDone                   chan struct{}
	claimTickerDone                    chan struct{}
	configDone                         chan error
	configLoopEnabled                  bool
	delegationTickerDone               chan struct{}
	fleetConnectorDone                 <-chan error
	fleetConnectorStarted              bool
	heartbeatDone                      <-chan struct{}
	journalGenerationCleanupErrors     *sweepErrorReporter
	mergedPRCostSweeps                 *mergedPRCostSweepRuntime
	migrationBackupCleanupErrors       *sweepErrorReporter
	readyNow                           bool
	recoveryInventoryTickerDone        <-chan struct{}
	sharedVisibilityDone               <-chan struct{}
	stalledTickerDone                  chan struct{}
	startupMergedPRCostSweepDone       <-chan struct{}
	startupRetentionSweepDone          <-chan struct{}
	startupTelemetryRetentionSweepDone <-chan struct{}
	startupTerminalFinalize            *startupTerminalFinalizer
	stopTerminalCleanupRetry           context.CancelFunc
	storageHealthTickerDone            <-chan struct{}
	supervisorStop                     chan error
	supervisorStopDone                 chan struct{}
	telemetryRetentionErrors           *sweepErrorReporter
	telemetryRetentionTickerDone       chan struct{}
	templateChecksDone                 <-chan struct{}
	templateNotices                    <-chan updateCheckResult
	terminalCleanupRetryDone           <-chan struct{}
	updateCheckDone                    <-chan struct{}
	updateNotices                      <-chan updateCheckResult
	worktreeRetentionTickerDone        chan struct{}
}

type upShutdown struct {
	apiFailed       bool
	configFailed    bool
	runErr          error
	schedulerFailed bool
	webhookFailed   bool
}

func (u *upSession) run() int {
	u.stdout = syncStartupStdout(u.stdout) // #4570
	// #4252: process-start reference point for logGateFlip's elapsed-time
	// readout on every named startup gate below.
	u.processStart = time.Now()
	var err error
	u.webhookGate, err = webhookhttp.NewDispatchGate(u.parentCtx)
	if err != nil {
		pf(u.stderr, "error: initialize daemon lifecycle: %v\n", err)
		return 1
	}
	u.ctx = u.webhookGate.Context()

	u.stopDaemon = func() {
		u.webhookGate.Stop()
	}
	parentBridgeDone := make(chan struct{})
	go func() {
		defer close(parentBridgeDone)
		select {
		case <-u.parentCtx.Done():
			u.stopDaemon()
		case <-u.ctx.Done():
		}
	}()
	defer func() {
		u.stopDaemon()
		<-parentBridgeDone
	}()

	return u.configure()
}

func (u *upSession) configure() int {
	var err error
	fs := newCLIFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(u.stderr)
	fs.Usage = helpUsage(u.stderr, "up")
	u.quiet = fs.Bool("quiet", false, "suppress periodic liveness heartbeats")
	u.diagnostics = fs.Bool("diagnostics", false, "capture deep per-stage diagnostics (process samples, lsof, un-truncated output) for hang debugging")
	u.watchConfig = fs.Bool("watch-config", true, "hot-reload materialized config-directory edits (default true; instance.yaml changes require restart)")
	u.drainTimeout = fs.Duration("drain-timeout", 0, "force shutdown if graceful drain exceeds this duration (default: wait indefinitely)")

	fs.Var(&u.notifications, "notify", "send desktop notifications for escalated and failed runs; use --notify=all for every terminal outcome")
	u.skipPreflight = fs.Bool("skip-preflight", false, "start despite instance config validation errors (unsafe)")
	u.cleanupSpansOnlyRuns = fs.Bool("cleanup-spans-only-runs", false, "delete reported legacy spans-only run directories at startup")
	u.disableReadModelReads = fs.Bool("disable-read-model-reads", false, "design §6.6 rollback: force authoritative journal scans for this run")
	if err := fs.Parse(u.args); err != nil {
		return 2
	}
	if *u.drainTimeout < 0 {
		pf(u.stderr, "error: --drain-timeout must not be negative\n")
		return 2
	}
	diagnosticsMode = *u.diagnostics
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	u.root = "."
	if fs.NArg() == 1 {
		u.root = fs.Arg(0)
	}

	// The shipped image runs the daemon as its container's pid 1, which makes
	// it the kernel's reparent target for every stage descendant that outlives
	// its parent — and a Go program waits for nothing but its own exec.Cmd
	// children, so those descendants would stay zombies for the life of the pod
	// (#3398). Install the missing init half before any stage can start. It is
	// pid-1-guarded, so a local `goobers up` is untouched and stays silent.
	if proc.StartOrphanReaper(u.ctx) {
		pf(u.stdout, "startup: running as container init (pid 1); reaping orphaned stage descendants\n")
	}

	u.l = instance.NewLayout(u.root)
	pf(u.stdout, "startup: validating instance configuration\n")
	if err := prepareDaemonStartupRoot(u.l, u.stderr); err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 2
	}
	if code := runStartupConfigPreflight(u.root, *u.skipPreflight, u.stderr); code != 0 {
		return code
	}
	pf(u.stdout, "startup: instance configuration valid\n")
	u.startupConfig, err = instance.LoadConfig(u.l.ConfigFile())
	if err != nil {
		pf(u.stderr, "error: invalid instance.yaml: %v\n", err)
		return 1
	}
	if warning := windowsLargeRepoEnvironmentWarning(u.startupConfig, u.l.WorkcopiesDir(), realWindowsLargeRepoPreflightDeps()); warning != "" {
		pln(u.stdout, warning)
	}
	u.livenessTimeout, err = u.startupConfig.Runner.LivenessTimeoutDuration()
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}

	// tracker/watchStartupReadiness (#4368): a daemon that is alive but stuck
	// somewhere between process start and API readiness looks identical to a
	// healthy one to every existing health signal — the scheduler heartbeat
	// this instance's own unhealthy classification (status.go) relies on
	// does not exist yet at this point in startup. The watchdog names the
	// current phase once livenessTimeout has passed without readiness,
	// giving an operator something to correlate a stuck dashboard/`status
	// --daemon` against instead of only a stale heartbeat.
	u.tracker = newStartupPhaseTracker(u.livenessTimeout)
	go watchStartupReadiness(u.ctx, u.stdout, u.tracker, u.ready.Load, u.livenessTimeout)
	u.retentionGate = &retentionSweepGate{}
	u.telemetryRetentionGate = &retentionSweepGate{}

	return u.serve()
}

func (u *upSession) serve() int {
	var err error
	// Single-instance lock (#23 AC3): a second `up` on the same instance root
	// must fail fast with a clear message, not silently race the first.
	if err := os.MkdirAll(u.l.SchedulerDir(), 0o755); err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 2
	}
	u.lockPath = filepath.Join(u.l.SchedulerDir(), "up.lock")
	u.priorLock, err = readPriorDaemonLock(u.lockPath)
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	behavior := buildStartupDaemonBehavior(u.stdout, u.startupConfig.RunConditions,
		*u.watchConfig, *u.diagnostics, *u.skipPreflight, *u.disableReadModelReads, *u.drainTimeout)
	release, err := acquireDaemonLock(u.lockPath, u.root, u.livenessTimeout, behavior)
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	defer release()
	u.currentDaemon, err = readCurrentDaemonIdentity(u.lockPath)
	if err != nil {
		pf(u.stderr, "error: read current daemon lock: %v\n", err)
		return 1
	}
	u.apiAddressPath = filepath.Join(u.l.SchedulerDir(), daemonAPIAddressFileName)
	if err := removeDaemonAPIAddress(u.apiAddressPath); err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	u.probes = &daemonProbeState{
		apiListening:            &u.apiListening,
		planeReady:              &u.planeReady,
		lastTriggerSweepAtNanos: &u.lastTriggerSweepAtNanos,
		startup:                 u.tracker,
		ready:                   &u.ready,
		configLoaded:            &u.configLoaded,
		stateOpen:               &u.stateOpen,
		resumeComplete:          &u.resumeComplete,
		sweepsStarted:           &u.sweepsStarted,
		schedulerTicked:         &u.schedulerTicked,
		lastTickAtNanos:         &u.lastTickAtNanos,
		livenessTimeout:         u.livenessTimeout,
		now:                     time.Now,
	}
	u.apiHandler, u.apiServer, u.apiLog, err = startStartupAPI(u.startupConfig, u.probes, u.tracker, u.apiAddressPath, u.stdout, u.stderr)
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	u.apiListening.Store(true)
	defer u.apiListening.Store(false)

	u.apiStopped = false
	defer func() {
		if u.apiStopped {
			return
		}
		u.stopDaemon()
		apiErr, webhookErr := shutdownHTTPServers(u.apiServer, u.webhookServer, httpShutdownGrace)
		if apiErr != nil {
			pf(u.stderr, "error: %v\n", apiErr)
		}
		if webhookErr != nil {
			pf(u.stderr, "error: shut down webhook listener: %v\n", webhookErr)
		}
	}()
	u.apiAddressPublished = true
	defer func() {
		if u.apiAddressPublished {
			if err := removeDaemonAPIAddress(u.apiAddressPath); err != nil {
				pf(u.stderr, "error: %v\n", err)
			}
		}
	}()
	u.tracker.set("scheduler-setup", u.root)

	return u.prepare()
}

func (u *upSession) prepare() int {
	var err error

	// DS6 (distributed-state-and-coordination.md §10): the gate keeps every
	// expired-claim reap — setup's included — a no-op until the renewal set
	// has been rebuilt from ledger + liveness below.
	u.claimRecoveryGate = localscheduler.NewRecoveryGate()
	var setupOptions []schedulerSetupOption
	setupOptions, u.startTelemetryReplay = daemonStartupSetupOptions(u.notifications, u.stdout, u.stderr, u.claimRecoveryGate, u.tracker)
	buildSetup := buildSchedulerSetup
	if *u.skipPreflight {
		buildSetup = buildSchedulerSetupAllowingInvalidConfig
	}
	u.setup, err = retryTransientStartup(u.ctx, u.stderr, func() (*schedulerSetup, error) { return buildSetup(u.ctx, u.l, &u.wg, setupOptions...) })
	if err != nil {
		return daemonStartupFailure(u.ctx, err, func() {
			printValidationIssues(u.stderr, validationReportFromError(err))
			pf(u.stderr, "error: initialize daemon scheduler: %v\n", err)
			if errors.Is(err, instance.ErrInvalidConfig) {
				instance.WriteInvalidConfigScope(u.stderr, u.l.ConfigDir(), validationReportFromError(err))
			}
		})
	}
	pf(u.stdout, "startup: scheduler initialized\n")
	// #4070: say, every start, whether one stage's memory is bounded. Stage
	// subprocesses share this daemon's memory cgroup, so an unbounded stage
	// can OOM-kill the control plane and take every in-flight run with it —
	// and the evidence self-erases (memory.events resets with the container,
	// so a post-hoc look reads oom_kill 0 on a pod killed 30 minutes earlier).
	// A structural hazard that no check fails on is one an operator can only
	// learn about from the daemon volunteering it.
	reportStageMemoryBound(u.setup.Config, u.stdout, u.stderr)
	// #3480: on a Windows host, say once whether the directories this daemon
	// writes then immediately reads are excluded from real-time scanning.
	// Advisory — startup continues regardless.
	//
	// Printed HERE, not beside the large-repo warning above, because the set
	// includes each gaggle's own workcopies.root — an override that beats the
	// instance-wide one and can point at any drive — and setup.Definitions is
	// the gaggle inventory the daemon itself is about to provision from. Read
	// off instance.yaml alone, the advisory would have reported an
	// affirmative all-clear over directories it never enumerated.
	if avDeps := realAVExclusionDeps(); avDeps.hostOS == "windows" {
		if line := hostAVExclusionAdvisory(u.ctx, "daemon",
			daemonAVExclusionDirectories(u.l, u.setup.Config, u.setup.Definitions, avDeps), avDeps); line != "" {
			pln(u.stdout, line)
		}
		if u.setup.Definitions == nil {
			pln(u.stdout, "av-exclusions (advisory, daemon): config directory unavailable; per-gaggle workcopies roots are NOT enumerated above")
		}
	}
	// #3806: instance config validated, definitions/scheduler wiring built.
	u.configLoaded.Store(true)
	logGateFlip(u.stdout, u.processStart, "configLoaded")
	u.storageGate, u.storageThresholds = startDaemonStorageHealth(u.setup)
	// #5343/#4911 AC5: the same reading, sampled on the daemon's own cadence,
	// feeds the read model AND the deduplicated high-water warning below.
	u.recoveryInventory = startDaemonRecoveryInventoryHealth(u.ctx, u.l, u.setup)
	// #3651: the normal stop path calls this explicitly below so a flush or
	// close failure fails the command; the defer only covers early returns,
	// and Shutdown itself runs at most once.
	u.shutdownSetup = func() error {
		err := u.setup.Shutdown(context.Background())
		if err != nil {
			pf(u.stderr, "error: shut down daemon services: %v\n", err)
		}
		return err
	}
	defer func() { _ = u.shutdownSetup() }()
	// Shared across the startup-triggered deferred sweep and the periodic
	// ticker below (#4373's "share the same exclusion mechanism"): one
	// error-rate-limiting reporter per failure class, not per call site.
	u.worktreeRetentionErrors = newSweepErrorReporter(u.setup.InstanceLog, "worktree_retention_sweep_failed")
	u.terminalCleanupRetryErrors = newSweepErrorReporter(u.setup.InstanceLog, "terminal_cleanup_retry_failed")
	if err := journalDaemonStart(u.setup.InstanceLog, u.priorLock, u.currentDaemon); err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	if err := journalValidationWarnings(u.setup.InstanceLog, u.setup.Validation.Warnings()); err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	// The blob plane (decision 010/012, §2a): a mode-3 stage pod's BlobClient
	// (internal/dispatcher/blob.go) fetches and puts content-addressed
	// artifacts by digest over this route instead of a shared filesystem. The
	// daemon fronts the SAME blobstore.Store type a local worker plugs into
	// MaterializeContext/StagingArtifacts (cmd/goobers/worker.go's
	// --blob-store), rooted at its own instance-local directory — wired
	// unconditionally like the claims and trigger planes, because it is inert
	// without a caller: a mode-1/2 daemon that never serves a mode-3 stage
	// never gets a request on these routes, and the blob plane's own
	// fail-closed pod-principal gate (registerBlobPlaneRoutes) keeps a
	// loopback null-auth daemon from handing out raw content to any local
	// caller either — the same posture the credential plane already takes.
	//
	// Constructed HERE, above the journal plane, because it is also the
	// daemon's SPAN SOURCE (#3805): the live journal writer and the DS5
	// reconciler both adopt an executor-recorded transcript by digest from
	// this store, and a stage pod PUTs the transcript into it over the same
	// plane. Its own HTTP service is registered further down, unchanged.
	u.blobStore, err = blobstore.NewDir(u.l.BlobStoreDir())
	if err != nil {
		pf(u.stderr, "error: initialize blob store: %v\n", err)
		return 1
	}
	// The live journal writer (DS4) authors engine-run journals from events
	// emitted as they happen; the projection reconciler below is thereby the
	// repair/verify path (DS5), never the authority, for live-authored runs.
	u.liveJournals, err = newLiveJournalWriter(u.l, u.setup.Config, u.setup.Definitions, u.setup.Watermarks, u.setup.InstanceLog, u.blobStore, u.setup.ProviderQuota)
	if err != nil {
		pf(u.stderr, "error: initialize live journal writer: %v\n", err)
		return 1
	}
	if u.liveJournals != nil {
		defer u.liveJournals.Close()
	}
	return u.startServices()
}

func (u *upSession) startServices() int {
	var err error
	// One Temporal client per daemon (decision 003 step 1(e)): the projection
	// reconciler, the DS6 claim-liveness probe and the engine-driven run
	// guards each used to dial the frontend themselves. nil on an instance
	// with no `engine:` configuration, which leaves every consumer below on
	// its pre-existing no-engine path.
	u.engineClient, err = newDaemonEngineClient(u.setup.Config)
	if err != nil {
		pf(u.stderr, "error: dial engine for daemon Temporal client: %v\n", err)
		return 1
	}
	defer u.engineClient.Close()
	u.engineGuards = u.engineClient.Guards()
	// blobStore is the SAME store the writer adopts spans from (#3805): DS5
	// verifies a live-authored journal against a re-projection, so a source
	// given to one and not the other turns every adopted span into a false
	// divergence.
	stopEngineProjection, err := startEngineProjection(u.ctx, u.l, u.setup.Config, u.setup.Definitions, u.engineClient, u.setup.Watermarks, u.setup.InstanceLog, u.setup.Telemetry, u.liveJournals, u.blobStore)
	if err != nil {
		return daemonStartupFailure(u.ctx, err, func() {
			pf(u.stderr, "error: start engine projection reconciler: %v\n", err)
		})
	}
	defer stopEngineProjection()
	// #3876 (decision 005 D1, piece 6): teach the guards the run-id ->
	// workflow-id mapping BEFORE anything reattaches, or a scheduled engine
	// run's describe returns NotFound and the resume scan releases its
	// concurrency slot underneath a live workflow. A failed scan is a
	// warning, not a boot failure: it degrades to the pre-#3876 behaviour, in
	// which direct runs still reattach correctly.
	var openEngineRuns map[string]engine.OpenRun
	var engineScanErr error
	u.engineGuards, openEngineRuns, engineScanErr = attachEngineOpenRunResolver(u.ctx, u.engineClient, u.engineGuards, ownedGaggleSet(u.setup.Machines))
	if daemonStartupWarning(u.ctx, engineScanErr, func() {
		pf(u.stderr, "warning: %v\n", engineScanErr)
	}) {
		return 0
	}
	for _, runID := range reportOrphanedEngineRuns(u.l, u.setup.InstanceLog, openEngineRuns) {
		pf(u.stderr, "warning: engine run %s is open on the engine with no local run directory\n", runID)
	}
	// #3876 (decision 005 D1): the engine starters the scheduler entries carry
	// were built before this client and this writer existed. Attach them now,
	// once, so a lane the selection predicate placed on the engine can
	// actually dispatch. An unattached runtime refuses the dispatch rather
	// than silently running remotely-pinned stages on this host.
	if u.engineClient != nil {
		u.setup.EngineRuntime.Attach(
			engine.NewTemporalStarter(u.engineClient.Temporal(), u.setup.Config.EffectiveEngineConfig().TaskQueue),
			u.engineGuards,
			u.liveJournals,
			time.Now,
		)
	}
	printValidationWarnings(u.stdout, u.setup.Validation.CLIWarnings())
	if warning := webhookConfigurationWarning(u.setup.Definitions, u.setup.Config); warning != "" {
		pln(u.stdout, warning)
	}
	if err := handleSpansOnlyRunCleanup(u.l, *u.cleanupSpansOnlyRuns, u.stdout); err != nil {
		pf(u.stderr, "error: clean up spans-only run directories: %v\n", err)
		return 1
	}

	u.reads, err = newDaemonReadService(readservice.LocalSources{
		Layout:      u.l,
		Config:      u.setup.Config,
		Definitions: u.setup.Definitions,
		Validation:  u.setup.Validation,
		Telemetry:   u.setup.RollupDB,
		// The read model, which this path did NOT attach until now.
		//
		// `goobers up` is the serving path — it is what answers the portal — and
		// it constructed the read service without a ReadModel, so read.db was
		// opened, migrated, built from journals, and kept current by the
		// projector while nothing ever read a row from it. Every list still took
		// the journal-derived path.
		//
		// That made all of Wave 2 inert in production: the cutover flag gates on
		// `sources.ReadModel != nil`, so turning it on would have changed
		// nothing here. Found by auditing which topologies attach which sources
		// (§13.1's "one read topology" is #1933; this is the concrete instance
		// of the divergence it exists to remove).
		ReadModel:                    u.setup.ReadModel,
		ChildHistory:                 u.readChildHistory,
		RetentionStats:               u.setup.RetentionStats,
		InstanceLogStats:             u.setup.InstanceLog.Stats,
		StorageHealthStats:           u.storageGate.Stats,
		TelemetryExporterHealthStats: u.setup.TelemetryExporterHealth.Snapshot,
		RecoveryInventoryStats:       u.recoveryInventory.Stats,
		WorkItemLookup:               statusWorkItemLookup(u.l.Root, u.setup.Definitions),
		SchedulerHeartbeat: func() (time.Time, error) {
			return daemonstate.Read(u.lockPath)
		},
		LivenessTimeout: u.livenessTimeout,
	}, u.ready.Load)
	if err != nil {
		pf(u.stderr, "error: initialize read service: %v\n", err)
		return 1
	}
	u.reads.AttachStartupStatus(func() *readservice.StartupStatus {
		return readserviceStartupStatus(u.tracker, u.ready.Load())
	})
	attachFreshnessSignals(u.reads, u.setup)
	if *u.disableReadModelReads {
		// The design §6.6 rollback, made operator-reachable (#2036):
		// DisableReadModelReads previously had no caller anywhere, so the
		// documented "a flag flip, never a deploy" rollback did not exist in
		// practice. read.db itself is untouched — this only forces every list
		// request back onto authoritative journal scans for this run.
		u.reads.DisableReadModelReads()
	}
	// Move the active-run count off the request path (#1741). Six read routes
	// used to walk every run directory in history and open every journal, per
	// request — 17.2 s cold on the live instance to answer "2" (design §2.1).
	// The daemon is the only construction that is long-lived enough for a
	// background sample to be warm, so it is the only one that starts it.
	stopActiveSampler := u.reads.StartActiveRunSampler(0)
	defer stopReadServiceWorker(stopActiveSampler, "active-run sampler", u.stderr)
	stopSchedulerProjector := u.reads.StartSchedulerStateProjector(0)
	defer stopReadServiceWorker(stopSchedulerProjector, "scheduler-state projector", u.stderr)
	u.stopDaemonHealth = startDaemonHealth(u.ctx, u.root, u.currentDaemon, u.setup, u.recoveryInventory.Stats, u.reads, u.ready.Load, u.engineClient)
	defer u.stopDaemonHealth()
	return u.configureAPI()
}

func (u *upSession) configureAPI() int {
	var err error
	// Unconfigured instances keep the tier-1 posture verbatim: null
	// authenticator, allow-all authorizer, plain HTTP on loopback. api.auth
	// swaps in the OIDC authenticator plus the role-floor authorizer, and
	// api.tls upgrades the transport (#640/#644).
	u.apiAuthorizer = httpapi.AllowAll
	// Live updates come from the change feed, and ONLY from the change feed
	// (#1929). The filesystem poller is deleted.
	//
	// A topology with no read model gets no SSE rather than a second detector.
	// That is the deliberate trade: the poller discovered change independently
	// of the projection, with different latency, completeness, and failure
	// modes, which is why an in-flight run was visible to one and not the other.
	// Keeping it as a fallback would preserve exactly the split section 8.1
	// exists to remove.
	//
	// A degraded topology already renders as degraded (#1928/#1933), so the
	// absence is reported rather than silent.
	u.apiHandlerOpts = daemonReadHandlerOptions(u.l.Root, u.setup)
	configReader, err := newConfigAuthoringReader(u.ctx, u.l, u.setup.Config)
	if err != nil {
		return reportDaemonStartupError(u.stderr, "initialize configuration source reader", err)
	}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithConfigAuthoringReader(configReader))
	u.interventions = newRunInterventionService(u.l, u.setup, &u.wg, u.apiLog)
	// #3883 (decision 005 R8): give the intervention surface a second
	// destination. Runner-driven runs keep the in-process path untouched;
	// engine-driven ones, which every verb refused outright since #3847, are
	// now answered by the workflow that owns them over the versioned HITL
	// protocol. Attached after the open-run scan so a SCHEDULED engine run —
	// whose workflow id is not its run id — is addressable; a daemon with no
	// engine client attaches nothing and keeps the refusal verbatim.
	if deliverer := u.engineClient.HITLDeliverer(u.engineGuards); deliverer != nil {
		u.interventions.AttachHITLDeliverer(deliverer)
	}
	// The write planes (#3509, distributed-state-and-coordination.md §7):
	// claims over the same ledger + flock the CLI claimants use, triggers
	// through the same scheduler path the pending-triggers sweep dispatches,
	// HITL resolution over the intervention machinery. The file seams remain
	// for local/mode-1 callers.
	u.triggerPlane = newDaemonTriggerService().withGaggleContainment(func(gaggle, runID string) bool {
		return runBelongsToGaggle(u.l, gaggle, runID)
	}).withSchedulerReadyGate(u.ready.Load).withStartupCatalog(u.setup.Entries)
	// The scheduler-state plane (#3878, decision 005 R3 / finding 002 C2):
	// the gaggle-scoped KV route for the scheduler state that is NOT a claim
	// — blocked.json, the backlog scan cursors, the reconcile-post-merge
	// ledger, the sibling-context cache. Served from the SAME files under the
	// SAME per-key locks the local CLI seams take (claims.lock for
	// blocked.json and the cursors), so a pod's compare-and-swap and a
	// runner-driven run's in-process update contend on one lock rather than
	// racing across two.
	var statePlane *daemonStateService
	u.durableTriggers, statePlane, u.cancelPlane, err = newDaemonCoordinationServices(u.l, u.triggerPlane, u.setup.RunnerRegistry, u.setup.InstanceLog)
	if err != nil {
		pf(u.stderr, "error: initialize daemon coordination planes: %v\n", err)
		return 1
	}
	defer func() { _ = u.durableTriggers.queue.Close() }()
	attachChildGenerationPins(u.setup.Generations, u.durableTriggers.queue)
	defer func() { _ = u.cancelPlane.receipts.Close() }()
	// The credential plane (#3511, distributed-state-and-coordination.md §11,
	// DS9/DS10): stage pods resolve short-lived, stage-scoped credentials at
	// stage start through the same capability-gated machinery the local
	// runner's executors resolve through. The snapshot is replaced on config
	// reload (see configreload.go) so a reloaded gaggle's grants apply.
	//
	// Wired on every daemon, but RULED fail-closed (PR #3528 finding 2): the
	// route itself requires an authenticated POD principal unconditionally —
	// on this file's loopback null-auth posture (no api.auth block, so no
	// authenticator below) every resolve answers a typed 403 rather than
	// handing raw secret material to any local caller. Local modes never need
	// the plane; their resolution stays in-process via buildCredentialEnv.
	u.credentialPlane = newDaemonCredentialService(u.l, u.setup.Config, u.setup.SecretStores, u.setup.SharedRegistry, u.setup.InstanceLog).withStageGrants(u.l.Root, u.apiServer.Address(), u.setup.Config.API.TLS != nil)
	// The startup methods tail-call through supervise/finish: this scope stays
	// alive for the daemon session, as do the queue-close defers above.
	defer unregisterDaemonStageGrants(u.l.Root, u.credentialPlane)
	if err := u.credentialPlane.enableChildWorkflows(u.durableTriggers.queue, u.setup.Definitions); err != nil {
		return reportDaemonStartupError(u.stderr, "initialize child workflow authority", err)
	}
	u.credentialPlane.childDispatch = u.triggerPlane
	u.credentialPlane.Replace(credentialPlaneDefinitionsFromSet(u.setup.Definitions))
	u.setup.CredentialPlane = u.credentialPlane
	if err := u.credentialPlane.installQueuedChildren(u.setup, u.durableTriggers, &u.wg); err != nil {
		return reportDaemonStartupError(u.stderr, "initialize queued child execution", err)
	}
	// The surrender plane (#3699) rides beside the blob store, under the same
	// instance-local root — the "<blob-store>/surrender" convention
	// cmd/goobers/workerdispatch.go's buildStageDispatch already documents
	// and constructs identically from its own --blob-store flag. When an
	// operator points a mode-3 `goobers worker --dispatch-namespace` at this
	// daemon's blob-store volume (the documented --dispatch-namespace
	// requirement), the two independently-built SurrenderDirs resolve to the
	// identical path and interoperate: the worker's activity reads what a
	// stage pod PUT here over HTTP. Wired unconditionally, like the blob
	// plane above — inert without a caller, and the surrender route's own
	// pod-principal gate (registerSurrenderPlaneRoutes) refuses everyone
	// else.
	surrenderStore, err := dispatcher.NewSurrenderDir(filepath.Join(u.l.BlobStoreDir(), "surrender"))
	if err != nil {
		pf(u.stderr, "error: initialize surrender plane: %v\n", err)
		return 1
	}
	// recoverExpiredClaims is the daemon's single stale-claim sweep, defined
	// once here so the claims plane's recover route (Goobers#4016) and the
	// startup/periodic call sites below all run the SAME sweep — with the
	// intervention predicate and the recovery gate applied — rather than two
	// sweeps that could drift apart. recoverClaims itself never touches
	// stdout/stderr: it returns the released entries so only the synchronous
	// startup call site below prints.
	u.recoverExpiredClaims = func(now time.Time) ([]localscheduler.ClaimEntry, error) {
		return recoverClaims(u.l, u.setup.InstanceLog, now, u.interventions.Active, u.claimRecoveryGate)
	}
	// The run-control plane routes local runs through the pending-cancels
	// sweep's live Runner path and retained engine runs through CancelWorkflow.
	// Only the local path uses the slot release attached below; the engine
	// releases its slot when its existing settlement path observes completion.
	u.cancelPlane.engine = newDaemonEngineCancelService(u.l, u.setup.Interventions, u.engineClient, u.engineGuards, u.setup.InstanceLog)
	claimPlane := newDaemonClaimService(u.l, u.setup.InstanceLog, u.recoverExpiredClaims)
	claimPlane.shared = daemonSharedClaimResolver(u.l, u.setup.Config, u.setup.SharedRegistry, u.setup.SecretStores)
	u.apiHandlerOpts = append(u.apiHandlerOpts, u.credentialPlane.installChildPodPlane(u.engineClient.Temporal(), surrenderStore, u.liveJournals, claimPlane, u.blobStore)...)
	journalService := newDaemonRunJournalService(u.l, u.setup.InstanceLog)
	journalService.reads = u.reads
	journalService.definitions = u.setup.Interventions
	withEngineOperatorMessageServices(journalService, u.liveJournals, u.engineClient, u.engineGuards)
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithRunJournalService(journalService), httpapi.WithOperatorMessageService(journalService))
	u.apiHandlerOpts = append(u.apiHandlerOpts,
		httpapi.WithInterventions(u.interventions),
		httpapi.WithInterventionContext(u.ctx),
		httpapi.WithClaimService(claimPlane),
		httpapi.WithTriggerService(u.durableTriggers),
		httpapi.WithEscalationService(intervention.NewEscalationResolver(u.interventions)),
		httpapi.WithCancelService(u.cancelPlane),
		httpapi.WithCredentialService(u.credentialPlane),
		httpapi.WithChildWorkflowService(u.credentialPlane.children.HTTPService()),
		httpapi.WithBlobService(u.credentialPlane.childBlobPlane(u.blobStore)),
		httpapi.WithRecoveryService(recoveryDeliveryService{layout: u.l, setup: u.setup}),
		httpapi.WithSurrenderService(surrenderStore),
		httpapi.WithStateService(statePlane),
		// The defect-nomination aggregate read (Goobers#4001). Wired
		// unconditionally, like the containment below: the four aggregates
		// are derived from this instance's own rollup by the same function
		// the CLI runs locally, so a daemon that can serve stage pods at all
		// can always answer them. An instance with no rollup answers "no
		// telemetry rollup yet", exactly as the local path does.
		httpapi.WithTelemetryDefectAggregateService(newDaemonTelemetryDefectAggregateService(u.l)),
		httpapi.WithPortalAssetHandler(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			serveDaemonInstanceAsset(response, request, u.l.Root)
		})),
		// The readiness-gate endpoint and the recovery gate it is exempt from
		// (#5019): wired unconditionally, like the containment above,
		// because every daemon build has a Layout and a startup phase
		// tracker regardless of which optional services below it configures.
		httpapi.WithInstanceReadinessService(&daemonInstanceReadinessService{instanceRoot: u.l.Root, tracker: u.tracker, ready: u.ready.Load}),
		httpapi.WithRecoveryGate(u.ready.Load),
		httpapi.WithRecoveryHints(u.tracker.setStartupHints),
	)
	if u.liveJournals != nil {
		// The journal plane (§8): remote stage pods emit their run's journal
		// events here; the daemon's own in-process emitters use the writer
		// directly and never pass through HTTP.
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithJournalService(u.liveJournals))
	}
	if instance.IsLoopbackListenAddress(apiListenAddress(u.setup.Config)) {
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithRunRevealer(runDirectoryRevealer(u.l)))
	}
	// The workflow-enable/disable mutation surface (WF-enable-disable): the
	// service is constructed here so the HTTP handler can be built before the
	// config reloader exists. AttachReloader wires the reloader in once it is
	// constructed below; until then the service fails closed with a documented
	// 503/workflow_mutations_unavailable envelope.
	u.workflowMutations = newWorkflowMutationService(u.l)
	u.apiHandlerOpts = append(u.apiHandlerOpts, workflowMutationHandlerOptions(u.workflowMutations)...)
	// The telemetry read plane's containment (decision 005 R4 / finding 002
	// C3). Wired unconditionally: without it every pod telemetry read is
	// refused, so this is what OPENS the plane, and a daemon that serves stage
	// pods at all can always answer which gaggle one of its own runs is in.
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithPodRunGaggle(podRunGaggleResolver(u.l)))
	// #4153: publish which config tree is in force, so a worker can detect that
	// its own has diverged instead of finding out when an agentic gate refuses.
	u.configDigests = newConfigDigestPublisher(u.setup.ConfigDigest)
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithConfigDigest(u.configDigests.Get))
	if u.apiHandlerOpts, err = appendWorkerDivergenceHandlerOption(u.apiHandlerOpts, u.setup); err != nil {
		return reportDaemonStartupError(u.stderr, "initialize worker config-divergence reporting", err)
	}
	return u.activateAPI()
}

func (u *upSession) activateAPI() int {
	var err error
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithTrustedProxies(u.setup.Config.API.TrustedProxies))
	// Pod-plane verifier: shared-key when configured (split daemon/dispatcher
	// deployments — Goobers#3701), else the daemon-local in-memory registry.
	podVerifier, perr := buildPodVerifier(u.setup.Config)
	if perr != nil {
		pf(u.stderr, "error: initialize pod token verifier: %v\n", perr)
		return 1
	}
	if auth := u.setup.Config.API.Auth; auth != nil && auth.OIDC != nil {
		authenticator, err := oidcauth.New(oidcauth.Config{
			Issuer:     auth.OIDC.Issuer,
			Audience:   auth.OIDC.Audience,
			RolesClaim: auth.OIDC.RolesClaimName(),
			Roles: oidcauth.RoleMapping{
				View:    auth.OIDC.Roles.View,
				Operate: auth.OIDC.Roles.Operate,
				Admin:   auth.OIDC.Roles.Admin,
			},
		})
		if err != nil {
			pf(u.stderr, "error: initialize HTTP API authenticator: %v\n", err)
			return 1
		}
		// Pod-to-daemon authn (#3509 §14 open point, resolved as per-run
		// minted bearers): chain the pod-token verifier in front of the human
		// OIDC authenticator. The registry is daemon-local (sound under DS1);
		// the mode-3 dispatcher mints into it at stage dispatch (#3482).
		chained, err := podauth.NewAuthenticator(podVerifier, authenticator)
		if err != nil {
			pf(u.stderr, "error: initialize HTTP API authenticator: %v\n", err)
			return 1
		}
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithAuthenticator(chained.WithCredentialGrants(u.credentialPlane.grantKey()).WithChildWorkflowGrants(u.credentialPlane.grantKey())))
		u.apiAuthorizer = httpapi.RequireRoles()
	} else if !instance.IsLoopbackListenAddress(apiListenAddress(u.setup.Config)) {
		// Non-loopback with no human authenticator configured: serve the pod
		// plane only, denying every non-pod request. This satisfies SEC-043's
		// requirement for a REAL authenticator without forcing an operator who
		// wants no human surface to stand up an OIDC issuer to get one
		// (Goobers#3701). It never admits an unauthenticated request — the
		// fallback denies rather than allowing.
		chained, err := podauth.NewAuthenticator(podVerifier, httpapi.DenyAllAuthenticator{})
		if err != nil {
			pf(u.stderr, "error: initialize HTTP API authenticator: %v\n", err)
			return 1
		}
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithAuthenticator(chained.WithCredentialGrants(u.credentialPlane.grantKey()).WithChildWorkflowGrants(u.credentialPlane.grantKey())))
		u.apiAuthorizer = httpapi.RequireRoles()
	}
	handler, err := httpapi.NewHandler(u.reads, u.apiAuthorizer, u.apiLog, u.apiHandlerOpts...)
	if err != nil {
		pf(u.stderr, "error: initialize HTTP API: %v\n", err)
		return 1
	}
	// #3806: /healthz and /readyz are registered OUTSIDE the versioned router
	// httpapi.NewHandler just built — no authenticate/authorize/admission/
	// budget — so a kubelet probe reaches them with no credential regardless
	// of api.auth (including this daemon's own DenyAllAuthenticator fallback
	// for a non-loopback bind with no human authenticator configured, just
	// above). Every other path keeps going through the versioned handler
	// exactly as before; WrapWithProbes forwards authenticatedTransport() and
	// shutdown() straight through so the server's SEC-043 posture and
	// apiHandler's own SSE-close lifecycle both keep working unchanged.
	handler = httpapi.WrapWithProbes(handler, u.probes.liveness, u.probes.readiness)
	// apiHandler.Set swaps the real versioned router in for startStartupAPI's
	// placeholder — the listener has been bound and serving since before
	// scheduler setup even began (#4999), so this is a hot swap, not a bind.
	// Everything but RouteInstanceReadiness (and RouteHealth) refuses with
	// 503 from here until crash-orphan Reap and every phase below completes
	// and `ready` flips true — the recovery gate in Router.serve (#5019).
	if err := u.apiHandler.Set(handler); err != nil {
		pf(u.stderr, "error: activate HTTP API: %v\n", err)
		return 1
	}
	// #4252: the full handler is live — credential/blob/journal/surrender
	// plane routes included (all constructed synchronously above, well
	// before this point, with any construction failure already returning 1)
	// — so a stage pod can now safely reach them, independent of
	// resumeComplete/sweepsStarted/ready below. This is what lets an
	// already-running stage pod keep working through the (unbounded) rest of
	// crash-resume instead of being held out of Service rotation for it.
	u.planeReady.Store(true)
	logGateFlip(u.stdout, u.processStart, "planeReady")
	return u.recoverClaims()
}

func (u *upSession) recoverClaims() int {
	var err error
	// Rebuild the claim-renewal set from the LEDGER plus run liveness before
	// any reap is permitted — DS6's load-bearing ordering
	// (distributed-state-and-coordination.md §10): this process's in-memory
	// run tracking is empty right now, but a distributed run dispatched by
	// the previous daemon process is still executing on the engine, and its
	// claims must be renewed — not reaped — across the restart. Only a
	// renewal pass whose ledger write completed opens the gate; a failed pass
	// leaves it closed and refuses startup: an expired lease is claimable
	// even with reaping gated, so crash resume must not execute without it.
	var closeClaimLiveness func()
	u.claimLiveness, closeClaimLiveness, err = buildClaimLivenessProbe(u.setup.Config, u.engineClient, u.setup.RunnerRegistry.RunIDs)
	if err != nil {
		pf(u.stderr, "error: build claim liveness probe: %v\n", err)
		return 1
	}
	defer closeClaimLiveness()
	if probeErr, renewErr := rebuildStartupClaimRenewalSet(u.ctx, u.l, u.claimLiveness, u.claimRecoveryGate); renewErr != nil {
		if daemonStartupStoppedByShutdown(u.ctx, renewErr) {
			return 0
		}
		if !isJournaledClaimsLockTimeout(renewErr) {
			pf(u.stderr, "error: rebuild claim renewal set before crash resume: %v\n", renewErr)
		}
		return 1
	} else if probeErr != nil {
		if daemonStartupStoppedByShutdown(u.ctx, probeErr) {
			return 0
		}
		pf(u.stdout, "warning: claim liveness probe degraded (renewed fail-live): %v\n", probeErr)
	}

	// Claim recovery (#131/#793): released once now and periodically thereafter
	// to recover expired leases and claim cleanup deferred by a terminal
	// finalizer's bounded lock timeout — before the scheduler starts admitting
	// new ticks, same ordering rationale as crash-resume below. withClaimLock
	// serializes this against a concurrent
	// `goobers backlog-query` subprocess claiming/releasing on the same
	// ledger file (providercmd.go's doc). The sweep itself is
	// recoverExpiredClaims, defined once above with the claims plane's
	// recover route so both run the same thing; the periodic goroutine below
	// deliberately does not print (see its own comment).
	startupReleased := append([]localscheduler.ClaimEntry(nil), u.setup.RecoveredClaims...)
	newlyReleased, err := u.recoverExpiredClaims(time.Now())
	if err != nil && !isJournaledClaimsLockTimeout(err) {
		pf(u.stderr, "error: recover expired claims: %v\n", err)
		return 1
	}
	startupReleased = append(startupReleased, newlyReleased...)
	for _, entry := range startupReleased {
		pf(u.stdout, "recovered expired claim %s (was held by run %s)\n", entry.ItemID, entry.RunID)
	}

	return u.recoverWorktrees()
}

func (u *upSession) recoverWorktrees() int {
	var err error
	// Once this daemon holds the instance lock, every stage-* entry belongs
	// to the prior process. Recover its provider receipts before removal;
	// scratch workspaces can contain durable effects even without git metadata.
	for gaggle, manager := range u.setup.WorktreesByGaggle {
		if err := runner.ReapScratchWorkspacesForRuns(filepath.Join(manager.Root, "scratch"), u.l.ForGaggle(gaggle).RunsDir()); err != nil {
			pf(u.stderr, "error: reap scratch workspaces for gaggle %s: %v\n", gaggle, err)
			return 1
		}
	}
	if u.setup.LegacyWorktrees != nil {
		if err := runner.ReapScratchWorkspacesForRuns(filepath.Join(u.setup.LegacyWorktrees.Root, "scratch"), u.l.RunsDir()); err != nil {
			pf(u.stderr, "error: reap legacy scratch workspaces: %v\n", err)
			return 1
		}
	}

	worktreeAccumulation, err := measureWorktreeAccumulation(u.setup.WorktreesByGaggle, u.setup.LegacyWorktrees)
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	u.tracker.setWorktreeAccumulation(worktreeAccumulation)
	budget := u.tracker.budgetSnapshot(time.Now())
	pf(u.stdout, "%s startup budget: worktrees=%d recovery-runs=%d total=%d budget=%s state=%s\n",
		startupTimestamp(), budget.Accumulation.Worktrees, budget.Accumulation.RecoveryRuns,
		budget.Accumulation.total(), budget.Budget, budget.State)

	// Reap crash-orphaned worktrees before anything tries to resume into one
	// of their keys (issue #136): a mid-stage crash otherwise leaves a
	// worktree directory that makes worktree.Create refuse forever (fixed
	// separately by adopt-and-reset, but Reap is still what actually reclaims
	// the disk space and the git worktree-list registration).
	//
	// cleanup-pending worktrees are already surrendered and have their own
	// bounded retry loop that starts immediately after readiness. Retrying the
	// entire durable queue here made restart time proportional to historical
	// cleanup failures, including entries whose handoff remains unavailable.
	for gaggle, manager := range u.setup.WorktreesByGaggle {
		manager := manager
		var warnings []worktree.ReapWarning
		reapErr := runStartupPhase(u.stdout, u.tracker, "worktree-reap-crash-orphan", gaggle, func() error {
			var reapErr error
			_, warnings, reapErr = manager.Reap(u.ctx, worktree.ReapOptions{
				DeferCleanupPending: true,
				IsRunTerminal:       worktreeRunTerminal(u.l.ForGaggle(gaggle).RunsDir()),
			})
			return reapErr
		})
		if reapErr != nil {
			return daemonStartupFailure(u.ctx, reapErr, func() {
				pf(u.stderr, "error: reap worktrees for gaggle %s: %v\n", gaggle, reapErr)
			})
		}
		for _, w := range warnings {
			pf(u.stdout, "warning: skipped worktree cleanup %s: %v\n", w.Path, w.Err)
		}
	}
	if u.setup.LegacyWorktrees != nil {
		var warnings []worktree.ReapWarning
		reapErr := runStartupPhase(u.stdout, u.tracker, "worktree-reap-crash-orphan", "legacy", func() error {
			var reapErr error
			_, warnings, reapErr = u.setup.LegacyWorktrees.Reap(u.ctx, worktree.ReapOptions{
				DeferCleanupPending: true,
				IsRunTerminal:       worktreeRunTerminal(u.l.RunsDir()),
			})
			return reapErr
		})
		if reapErr != nil {
			return daemonStartupFailure(u.ctx, reapErr, func() {
				pf(u.stderr, "error: reap legacy worktrees: %v\n", reapErr)
			})
		}
		for _, w := range warnings {
			pf(u.stdout, "warning: skipped worktree cleanup %s: %v\n", w.Path, w.Err)
		}
	}
	// Broad worktree/branch retention (merged-local-branch pruning etc.) is
	// recoverable housekeeping, not the correctness-critical crash-orphan
	// recovery just above — resuming into a stale worktree key would be a
	// correctness bug, but a kept failure worktree or a fully-merged local
	// branch sitting around one sweep interval longer is not. Running it
	// synchronously here previously blocked API readiness on however long a
	// large cleanup backlog took (#4373: 166 merged branches took roughly 11
	// minutes on MDB5, during which `status --daemon` and the dashboard both
	// treated the daemon as unreachable). It now runs as a background sweep
	// gated on readiness instead (right after ready.Store(true), below),
	// coalescing with the periodic 6h sweep via retentionGate so at most one
	// ever runs at a time.
	pf(u.stdout, "%s startup phase=retention-sweep status=deferred target=%q\n", startupTimestamp(), "runs after API readiness, not before (#4373)")
	u.telemetryRetentionConfig, u.migrationBackupGaggles = configuredTelemetryRetention(u.setup)
	if telemetryErr := reconcileStartupTelemetryRetention(u.stdout, u.tracker, u.l, u.setup); telemetryErr != nil {
		pf(u.stderr, "error: reconcile retained telemetry: %v\n", telemetryErr)
		return 1
	}
	pf(u.stdout, "%s startup phase=telemetry-retention-prune status=deferred target=%q\n", startupTimestamp(), "runs after API readiness, not before (#5233)")

	// Prune crash-abandoned orphan runs and run-creation staging directories
	// before anything else touches the runs tree (#2035): a mid-Create crash's
	// os.RemoveAll cleanup is in-process only, so a `.runs.creating` residue
	// otherwise sits until an operator happens to run `goobers telemetry
	// prune-orphans` — the same gap worktree Reap (above) and telemetry
	// retention (immediately above) already close for their own trees.
	var orphansPruned []retention.OrphanResult
	orphanErr := runStartupPhase(u.stdout, u.tracker, "orphan-run-prune", "", func() error {
		var pruneErr error
		orphansPruned, pruneErr = pruneOrphansAtStartup(u.l, time.Now())
		return pruneErr
	})
	if orphanErr != nil {
		pf(u.stderr, "error: prune orphan run directories: %v\n", orphanErr)
		return 1
	}
	for _, result := range orphansPruned {
		source := "run"
		if result.CreationStage {
			source = "creation-stage"
		}
		pf(u.stdout, "pruned orphan run directory name=%q source=%s path=%q lastModified=%s\n",
			result.Name, source, result.RunDir, result.LastModified.UTC().Format(time.RFC3339))
	}
	return u.startScheduler()
}

func (u *upSession) startScheduler() int {
	var err error

	// Reconcile BEFORE the resume scan (issue #135): it seeds Conditions'
	// active-run counts from the very same non-terminal runs the resume scan
	// is about to act on, so each resumed run's ReleaseReconciled call (below)
	// has a reserved slot to actually release.
	// markTickProgress updates the two in-memory values daemonProbeState's
	// liveness check reads (#3806): purely an atomic store, never disk I/O,
	// so it stays safe to call frequently — including from inside Tick's
	// tickMu-held critical section, below — even on this cluster's
	// documented failure mode of a stalled RWO volume attachment.
	markTickProgress := func(tickAt time.Time) {
		u.lastTickAtNanos.Store(tickAt.UnixNano())
		// #3806: /healthz's liveness grace ends once the scheduler has ticked
		// at least once — set regardless of whether the on-disk heartbeat
		// write below succeeds, since the tick itself (not that write) is
		// what "has the main loop reached its steady-state loop" means.
		u.schedulerTicked.Store(true)
	}
	u.sched = newDaemonScheduler(u.setup,
		localscheduler.WithTickHeartbeat(u.livenessTimeout/2, func(tickAt time.Time) error {
			err := daemonstate.Refresh(u.lockPath, tickAt)
			markTickProgress(tickAt)
			return err
		}),
		// #3806: a single Tick can poll several due, provider-backed
		// workflows SEQUENTIALLY while holding tickMu (each bounded only by
		// demandPollTimeout, 45s) — WithTickHeartbeat's refresh above fires
		// only once Tick returns in full, which for N due polls can leave
		// the liveness heartbeat looking stale for N*45s even though the
		// scheduler is busy, not wedged. WithPollHeartbeat marks progress
		// after EACH such poll instead, bounding staleness to a single
		// poll's worst case.
		localscheduler.WithPollHeartbeat(markTickProgress),
		localscheduler.WithDiskGate(u.storageGate),
	)
	u.sourceReconcileWake = make(chan struct{}, 1)
	wakeSourceReconcile := func(context.Context) {
		select {
		case u.sourceReconcileWake <- struct{}{}:
		default:
		}
	}
	u.interventions.AttachScheduler(u.sched)
	u.triggerPlane.AttachScheduler(u.sched)
	// #3876: runs the trigger plane mints outlive the HTTP request that asked
	// for them. Admission is still validated against the request context.
	u.triggerPlane.AttachDispatchContext(u.ctx)
	webhookLog := log.New(u.stderr, "webhook: ", log.LstdFlags)
	u.webhookServer, err = buildWebhookServer(u.ctx, u.setup, u.sched, u.webhookGate, webhookLog, wakeSourceReconcile)
	if err != nil {
		return daemonStartupFailure(u.ctx, err, func() {
			pf(u.stderr, "error: %v\n", err)
		})
	}
	u.recoveryRunDirs, err = reconcileStartupRuns(u.ctx, u.l, u.setup, u.sched, u.tracker, u.stdout)
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	// #3806: the scheduler's run-tracking state has been reconciled from the
	// run directories already on disk.
	u.stateOpen.Store(true)
	logGateFlip(u.stdout, u.processStart, "stateOpen")
	stalledRunTimeout, err := u.setup.RunConditions.StalledRunTimeoutDuration()
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	maxRunDuration, err := u.setup.RunConditions.MaxRunDurationDuration()
	if err != nil {
		pf(u.stderr, "error: %v\n", err)
		return 1
	}
	u.stalledSweepErrors = newSweepErrorReporter(u.setup.InstanceLog, "stalled_run_sweep_failed")
	drainedDowntime, hostSuspensions := readSweepDowntime(u.setup.InstanceLog, u.stderr)
	sweepDeps := stalledSweepDependencies(u.setup, drainedDowntime, hostSuspensions, u.liveJournals)
	u.sweepStalled = func(now time.Time, recoveryRunDirs ...[]string) error {
		// Observe first, so a suspension that just ended is credited by the
		// sweep that would otherwise read it as run time or silence.
		observeErr := hostSuspensions.Observe(now)
		return errors.Join(observeErr, sweepStalledRuns(
			u.ctx,
			u.l,
			u.setup.RunnerRegistry,
			u.setup.LegacyRunner,
			u.engineGuards,
			u.setup.InstanceLog,
			sweepDeps,
			u.setup.TerminalNotifier,
			u.sched.ReleaseRun,
			now,
			stalledRunTimeout,
			maxRunDuration,
			recoveryRunDirs...,
		))
	}
	// Reap stale journals before crash-resume can refresh them with a new
	// stage heartbeat.
	u.stalledSweepErrors.report(runStartupPhase(u.stdout, u.tracker, "stalled-run-reconcile", fmt.Sprintf("candidates=%d", len(u.recoveryRunDirs)), func() error {
		return u.sweepStalled(time.Now(), u.recoveryRunDirs)
	}))

	u.cancelPlane.AttachRelease(u.sched.ReleaseRun)

	// api-bind itself runs much earlier (#4999's startStartupAPI, before
	// scheduler setup even begins), and apiHandler.Set(handler) above already
	// swaps in the real versioned router before crash-orphan Reap runs below
	// — the recovery gate in Router.serve (#5019) is what keeps every route
	// but RouteInstanceReadiness (and RouteHealth) unavailable in between.
	if u.webhookServer != nil {
		if err := runStartupPhase(u.stdout, u.tracker, "webhook-listener-start", u.webhookServer.Address(), u.webhookServer.Start); err != nil {
			pf(u.stderr, "error: start webhook listener: %v\n", err)
			return 1
		}
	}

	u.openPRs = newOpenPRLoop(u.ctx, u.setup.OpenPRRefresher)
	defer u.openPRs.Stop()
	u.cleanupRetries = newTerminalCleanupRetryRegistry(u.setup)
	u.setup.MergedPRCostReconciler = newDaemonMergedPRCostReconciler(
		u.setup.Root,
		u.setup.Config,
		u.setup.Definitions,
		u.setup.SecretStores,
		u.setup.SharedRegistry,
		u.setup.ProviderQuota,
	)

	// The reloader is always constructed (not gated behind --watch-config)
	// because `goobers apply` (#459) needs to run exactly one reload check
	// on demand regardless of whether continuous watching is enabled — the
	// flag only decides whether its own ticker loop (wired further below)
	// runs automatically.
	u.reloader = &configReloader{
		watching:       *u.watchConfig,
		layout:         u.l,
		setup:          u.setup,
		scheduler:      u.sched,
		openPRs:        u.openPRs,
		reads:          u.reads,
		cleanupRetries: u.cleanupRetries,
		readModel:      u.setup.ReadModel,
		wg:             &u.wg,
		appliedDigest:  u.setup.ConfigDigest,
		observedDigest: u.setup.ConfigDigest,
		digests:        u.configDigests,
	}
	// The workflow mutation service was built above so the HTTP handler could
	// register the surface before the reloader existed. Now that it does,
	// attach it so subsequent SetWorkflowEnabled calls can drive on-demand
	// pollOnce reloads and roll back their on-disk edit on rejection.
	u.workflowMutations.AttachReloader(u.reloader)
	stopConfigMirror := u.reloader.startConfigMirror(u.ctx)
	defer stopConfigMirror()
	u.reloader.publishReloadStatus(time.Now())

	return u.recoverRuns()
}

func (u *upSession) recoverRuns() int {
	var err error
	// #3969/#4420 follow-up: this MUST run before crash-resume below, not
	// after. Resuming a run dispatches its next stage in a background
	// goroutine tracked by wg (joined far later, near shutdown) — if the
	// sweep ran after resume started, a resumed run's stage could call
	// ephemeraltmp.Establish and create a brand-new, live
	// goobers-ephemeral-tmp-* directory before or during the sweep, which
	// SweepOrphans cannot distinguish from a genuine prior-generation orphan
	// and would delete out from under an in-flight build. Running it here
	// preserves the invariant SweepOrphans' own doc requires: called before
	// this process has established any Scope of its own.
	reconcileStartupEphemeralTemp(u.setup, u.tracker, u.stdout)

	// Crash-resume: any run left non-terminal by a prior crash or unclean
	// shutdown restarts now, before the scheduler starts admitting new ticks
	// (#23 AC: restart via Runner.Resume). A run whose workflow no longer
	// resolves in config is skipped with a warning (issue #135), not fatal —
	// recover it with `goobers run abort <run-id>`. Each resumed run also
	// incrementally ingests into the telemetry rollup once its outcome is
	// known (issue #127).
	u.resumeResult, err = resumeStartupRuns(u.ctx, u.l, u.setup, u.engineGuards, u.sched, &u.wg, u.recoveryRunDirs, u.tracker, u.stdout)
	if err != nil {
		return daemonStartupFailure(u.ctx, err, func() {
			pf(u.stderr, "error: %v\n", err)
		})
	}
	for _, runID := range u.resumeResult.Resumed {
		pf(u.stdout, "resuming interrupted run %s\n", runID)
	}
	// An engine-driven run is NOT resumed: this daemon waits for the engine's
	// workflow and echoes its outcome. Announced separately so an operator
	// reading the startup log can tell the two apart at a glance.
	for _, runID := range u.resumeResult.Reattached {
		pf(u.stdout, "re-attaching to engine-driven run %s\n", runID)
	}
	// Renew resumed runs' claims immediately rather than waiting up to
	// claimRecoverInterval for the first periodic tick (#2014): the startup
	// recovery sweep used startup-only durable local journal evidence before
	// resume tracked anything. Refresh that grace after the recovery work,
	// using only actual execution liveness from this point onward. The resumed
	// runs are tracked by the registry now, so the ledger-driven pass covers
	// exactly them (plus any engine-live holders — idempotent). Best-effort,
	// same as the periodic sweep: a renewal failure here does not fail daemon
	// start, since the claim ledger's own reap is what it would fail open to.
	if len(u.resumeResult.Resumed) > 0 {
		renewErr := renewResumedClaimsAtStartup(u.ctx, u.l, u.claimLiveness, len(u.resumeResult.Resumed), u.tracker, u.stdout)
		if isJournaledClaimsLockTimeout(renewErr) {
			renewErr = nil
		}
		if daemonStartupWarning(u.ctx, renewErr, func() {
			// renewErr is non-nil whenever the reporter runs.
			pf(u.stdout, "warning: renew resumed claims: %v\n", renewErr)
		}) {
			return 0
		}
	}
	for _, runID := range u.resumeResult.Warned {
		pf(u.stdout, "warning: run %s references a workflow no longer in config — skipped; recover with `goobers run abort %s`\n", runID, runID)
	}
	// #3806: crash-resume of every interrupted run in the non-terminal
	// inventory finished. Its duration scales with genuinely recoverable work,
	// so a kubelet startupProbe against /readyz must still allow enough time
	// for those runs, not for retained terminal history.
	// #5199 made that claim true: see startStartupTerminalFinalize below.
	u.resumeComplete.Store(true)
	logGateFlip(u.stdout, u.processStart, "resumeComplete")

	// Sweep once before announcing readiness so requests and responses orphaned
	// across daemon lifetimes are handled without waiting for the first tick.
	u.triggerSweepErrors = newSweepErrorReporter(u.setup.InstanceLog, "trigger_sweep_failed")
	u.triggerSweep = daemonTriggerSweep(u.ctx, u.l, u.setup.InstanceLog, u.durableTriggers, u.sched, &u.lastTriggerSweepAtNanos, triggerSweepOptions{})
	startupTriggerSweep := daemonTriggerSweep(u.ctx, u.l, u.setup.InstanceLog, u.durableTriggers, u.sched, &u.lastTriggerSweepAtNanos, triggerSweepOptions{
		staleLegacyMissingDeadline: true,
		recoverActiveRequests:      true,
	})
	u.triggerSweepErrors.report(runStartupPhase(u.stdout, u.tracker, "trigger-request-reconcile", "", startupTriggerSweep))
	claimAdminSweepErrors := newSweepErrorReporter(u.setup.InstanceLog, "claim_admin_sweep_failed")
	claimAdminSweepErrors.report(reconcileStartupClaimAdmin(u.l, u.setup, u.recoverExpiredClaims, u.tracker, u.stdout))
	u.stopClaimAdminSweep = startClaimAdminSweep(u.l, u.setup.InstanceLog, u.recoverExpiredClaims, claimAdminSweepErrors)
	defer u.stopClaimAdminSweep()
	// #831's daemon-side half: cancel one live in-flight run on operator request
	// by resolving its owning Runner and calling CancelRun. Its own ticker (below)
	// keeps a worst-case wedged-stage cancellation — which blocks in CancelRun for
	// the cancellation + terminalization grace — from stalling the trigger/claim
	// sweeps that share the delegation ticker.
	u.cancelSweepErrors = newSweepErrorReporter(u.setup.InstanceLog, "cancel_sweep_failed")
	u.cancelSweep = func() error {
		return sweepPendingCancelRequests(u.l.SchedulerDir(), u.setup.RunnerRegistry, u.setup.InstanceLog, u.sched.ReleaseRun, time.Now, u.cancelPlane.fenceChildren)
	}
	u.cancelSweepErrors.report(runStartupPhase(u.stdout, u.tracker, "cancel-request-reconcile", "", u.cancelSweep))

	return u.configureSource()
}

func (u *upSession) configureSource() int {
	// #459's daemon-side half: on operator request (`goobers apply`), run
	// exactly one config-reload check now instead of waiting for
	// --watch-config's own ticker (or performing one at all if it's off). For
	// a git-tracked workflowSource, first pull the tracked ref's latest
	// commit into the config directory — the same validate-or-keep-LKG
	// contract reloader.pollOnce already enforces for a hand-edited file
	// applies unchanged to a git-sourced one.
	u.applySweepErrors = newSweepErrorReporter(u.setup.InstanceLog, "apply_sweep_failed")
	// #3274: a github-app workflowSource mints installation tokens instead of
	// reading a static token ref. The minter is built once here — the
	// composition root, since internal/instance cannot import
	// internal/githubapp — and shared by the apply sweep and the reconcile
	// loop below, so both draw on one near-expiry-refreshing token cache.
	var workflowSourceAppTokens instance.GitTokenSource
	if source := u.setup.Config.WorkflowSource; source != nil && source.GitHubAppAuth() {
		minted, mintErr := newWorkflowSourceAppTokenSource(*source, u.setup.SharedRegistry, u.setup.SecretStores)
		if mintErr != nil {
			pf(u.stderr, "error: configure workflow-source GitHub App authentication: %v\n", mintErr)
			return 1
		}
		workflowSourceAppTokens = minted
	}

	if source := u.setup.Config.WorkflowSource; source != nil && source.Kind == instance.WorkflowSourceKindGit {
		u.sourceApplier = &workflowSourceApplier{
			root: u.root, source: *source, appTokens: workflowSourceAppTokens, setup: u.setup, reloader: u.reloader,
		}
	}
	reconcileApply := func(applyCtx context.Context, now time.Time) applyResponse {
		if u.sourceApplier != nil {
			return u.sourceApplier.Apply(applyCtx, now)
		}
		var resp applyResponse
		applied, oldDigest, newDigest, rejected, reloadErr := u.reloader.pollOnce(now)
		resp.Applied = applied
		resp.OldDigest = oldDigest
		resp.NewDigest = newDigest
		resp.Rejected = rejected
		if reloadErr != nil {
			resp.Error = reloadErr.Error()
		}
		return resp
	}
	u.applySweep = func() error {
		return sweepPendingApplyRequests(u.ctx, u.l.SchedulerDir(), reconcileApply, time.Now)
	}
	u.applySweepErrors.report(runStartupPhase(u.stdout, u.tracker, "apply-request-reconcile", "", u.applySweep))

	return u.startMaintenance()
}

func (u *upSession) startMaintenance() int {
	// The periodic sweep runs on its own goroutine for the daemon's entire
	// lifetime, concurrently with the main goroutine's own stdout/stderr
	// writes (both "daemon started" above and the shutdown messages below) —
	// io.Writer implementations like *bytes.Buffer (tests) are not safe for
	// concurrent use, so this goroutine deliberately never writes to
	// stdout/stderr itself (unlike the startup sweep above, which runs
	// synchronously before this goroutine exists and so writes safely).
	// Failures and non-empty recoveries go to the concurrency-safe instance
	// journal instead.
	claimTicker := time.NewTicker(claimRecoverInterval)
	u.claimTickerDone = make(chan struct{})
	claimSweepErrors := newSweepErrorReporter(u.setup.InstanceLog, "claim_recovery_failed")
	claimRenewErrors := newSweepErrorReporter(u.setup.InstanceLog, "claim_renewal_failed")
	go func() {
		defer close(u.claimTickerDone)
		defer claimTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case now := <-claimTicker.C:
				// Renew before reaping (#2014/DS6): both run in the same tick,
				// and a live run's lease must be pushed back into the future
				// before recoverExpiredClaims below checks it against now — doing
				// it in the other order would let a run that is still live get
				// reaped on the exact tick its lease was due to be renewed, on
				// nothing worse than ordinary ticker jitter.
				// rebuildClaimRenewalSet also self-heals DS6's startup ordering:
				// if the startup rebuild failed (gate still closed), a completed
				// pass here IS the rebuild — recovery below is permitted from
				// here on.
				probeErr, renewErr := rebuildClaimRenewalSet(u.ctx, u.l, u.claimLiveness, u.claimRecoveryGate)
				if isJournaledClaimsLockTimeout(renewErr) {
					claimRenewErrors.report(nil)
				} else if renewErr != nil {
					claimRenewErrors.report(renewErr)
				} else {
					claimRenewErrors.report(probeErr)
				}
				released, err := u.recoverExpiredClaims(now)
				if isJournaledClaimsLockTimeout(err) {
					claimSweepErrors.report(nil)
				} else {
					claimSweepErrors.report(err)
				}
				if err == nil && len(released) > 0 {
					u.setup.InstanceLog.AppendBestEffort(journal.Event{
						Type:   journal.EventClaimReleased,
						Reason: fmt.Sprintf("periodic recovery released %d expired claim(s)", len(released)),
						Runner: map[string]any{"releasedClaims": len(released)},
					})
				}
			}
		}
	}()

	stalledTicker := time.NewTicker(stalledRunSweepInterval)
	u.sharedVisibilityDone = startSharedVisibilityReconciler(u.ctx, u.l, u.setup.InstanceLog)
	u.stalledTickerDone = make(chan struct{})
	go func() {
		defer close(u.stalledTickerDone)
		defer stalledTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case now := <-stalledTicker.C:
				u.stalledSweepErrors.report(u.sweepStalled(now))
			}
		}
	}()

	telemetryRetentionTicker := time.NewTicker(telemetryRetentionSweepInterval)
	u.telemetryRetentionTickerDone = make(chan struct{})
	u.telemetryRetentionErrors = newSweepErrorReporter(u.setup.InstanceLog, "telemetry_retention_sweep_failed")
	u.migrationBackupCleanupErrors = newSweepErrorReporter(u.setup.InstanceLog, "migration_backup_cleanup_failed")
	// Stale journal-generation cleanup is diagnostic, not fatal: it gets its
	// own reporter so a stranded generation is journaled without failing the
	// retention sweep that otherwise succeeded (#3654).
	u.journalGenerationCleanupErrors = newSweepErrorReporter(u.setup.InstanceLog, "journal_generation_cleanup_failed")
	go func() {
		defer close(u.telemetryRetentionTickerDone)
		defer telemetryRetentionTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case now := <-telemetryRetentionTicker.C:
				runGatedTelemetryRetentionSweep(u.ctx, u.l, u.setup, u.migrationBackupGaggles, u.telemetryRetentionConfig, u.telemetryRetentionGate, u.telemetryRetentionErrors, u.journalGenerationCleanupErrors, u.migrationBackupCleanupErrors, now)
			}
		}
	}()

	// #2052's fix: Manager.Reap and pruneConfiguredRetention previously ran
	// only in the synchronous startup block above, so a crash orphan or a
	// kept failure worktree that appeared after startup sat until the next
	// restart. This ticker re-runs both on the same never-write-to-stdout
	// footing as the tickers above (writers are io.Discard here since a
	// periodic sweep has no interactive caller to report progress to;
	// failures still reach the instance journal via the error reporter).
	worktreeRetentionTicker := time.NewTicker(worktreeRetentionSweepInterval)
	u.worktreeRetentionTickerDone = make(chan struct{})
	go func() {
		defer close(u.worktreeRetentionTickerDone)
		defer worktreeRetentionTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case <-worktreeRetentionTicker.C:
				err := u.retentionGate.run(func() error {
					return sweepWorktreeRetention(u.ctx, u.l, u.setup)
				})
				if !errors.Is(err, errRetentionSweepAlreadyRunning) {
					u.worktreeRetentionErrors.report(err)
				}
			}
		}
	}()

	u.storageHealthTickerDone = startStorageHealthTicker(u.ctx, u.setup, u.storageGate, u.storageThresholds.CheckInterval)
	u.recoveryInventoryTickerDone = startRecoveryInventoryTicker(u.ctx, u.l, u.setup, u.recoveryInventory, recoveryInventorySampleInterval)
	u.mergedPRCostSweeps = startMergedPRCostSweepRuntime(u.ctx, u.setup)

	u.apiReadCacheLockSweepTickerDone = startAPIReadCacheLockSweepTicker(u.ctx, u.l)

	return u.startRequestLoops()
}

func (u *upSession) startRequestLoops() int {
	// #343's daemon-side half: periodically sweep for delegated trigger
	// requests a short-lived `goobers run` invocation dropped after finding
	// this daemon already holding up.lock (rundelegate.go), and dispatch
	// each through sched.Trigger — safe to call concurrently with sched.Run's
	// own Tick loop below (Scheduler's internal mutex already makes
	// Trigger/Tick safe to interleave, see scheduler.go's Tick doc comment;
	// this is exactly that same sanctioned pattern, just from a second
	// goroutine instead of a second process). Same never-write-to-stdout
	// rationale as the claim-recovery goroutine above.
	delegationTicker := time.NewTicker(delegationSweepInterval)
	u.delegationTickerDone = make(chan struct{})
	go func() {
		defer close(u.delegationTickerDone)
		defer delegationTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case <-delegationTicker.C:
				u.triggerSweepErrors.report(u.triggerSweep())
			}
		}
	}()

	// #831's cancel sweep runs on its own ticker so a slow (wedged-stage)
	// cancellation never delays the trigger/claim delegation sweeps above.
	cancelTicker := time.NewTicker(delegationSweepInterval)
	u.cancelTickerDone = make(chan struct{})
	go func() {
		defer close(u.cancelTickerDone)
		defer cancelTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case <-cancelTicker.C:
				u.cancelSweepErrors.report(u.cancelSweep())
			}
		}
	}()

	// #459's apply sweep runs on its own ticker for the same reason cancel's
	// does: a slow reconcile (a remote git fetch) must never delay the
	// trigger/claim delegation sweeps it shares no ticker with.
	applyTicker := time.NewTicker(delegationSweepInterval)
	u.applyTickerDone = make(chan struct{})
	go func() {
		defer close(u.applyTickerDone)
		defer applyTicker.Stop()
		for {
			select {
			case <-u.ctx.Done():
				return
			case <-applyTicker.C:
				u.applySweepErrors.report(u.applySweep())
			}
		}
	}()
	// #3806: the initial synchronous trigger/claim-admin/cancel/apply sweeps
	// above already ran once, and every one of their periodic tickers is now
	// live.
	u.sweepsStarted.Store(true)
	logGateFlip(u.stdout, u.processStart, "sweepsStarted")

	u.supervisorStop = make(chan error, 1)
	u.supervisorStopDone = make(chan struct{})
	go func() {
		defer close(u.supervisorStopDone)
		ticker := time.NewTicker(delegationSweepInterval)
		defer ticker.Stop()
		for {
			requested, err := selfupdate.ConsumeStopRequest(u.root)
			if err != nil || requested {
				u.supervisorStop <- err
				return
			}
			select {
			case <-u.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Plain materialized-directory watching remains opt-in. A Git workflow
	// source reconciles continuously: polling is the availability floor and a
	// local ref watcher provides low-latency wakeups.
	u.configDone = make(chan error, 1)
	u.configLoopEnabled = *u.watchConfig
	if u.sourceApplier != nil {
		u.configLoopEnabled = true
		sourceLoop := &configSourceReconciler{
			source:    u.sourceApplier.source,
			errors:    newSweepErrorReporter(u.setup.InstanceLog, "config_reconcile_failed"),
			wake:      u.sourceReconcileWake,
			reconcile: u.sourceApplier.Reconcile,
		}
		go func() { u.configDone <- sourceLoop.Run(u.ctx) }()
	} else if *u.watchConfig {
		go func() { u.configDone <- u.reloader.Run(u.ctx) }()
	}

	return u.readyForWork()
}

func (u *upSession) readyForWork() int {
	if err := prepareDaemonReadiness(u.ctx, u.reads, u.tracker, u.stdout); err != nil {
		// A signal before readiness is still a clean daemon shutdown. The same
		// cancellation after readiness already exits 0 below; preserve that
		// documented contract across the startup boundary (#4875).
		return daemonStartupFailure(u.ctx, err, func() {
			pf(u.stderr, "error: %v\n", err)
		})
	}
	var fleetConnectorErr error
	u.fleetConnectorDone, u.fleetConnectorStarted, fleetConnectorErr = startFleetConnectorPhase(u.ctx, u.root, u.tracker, u.stdout)
	if fleetConnectorErr != nil {
		pf(u.stdout, "warning: Fleet connector unavailable: %v\n", fleetConnectorErr)
	}
	u.readyNow = u.webhookGate.Start()
	if u.readyNow {
		u.tracker.completeBudget(time.Now())
		u.ready.Store(true)
		pf(u.stdout, "%s startup phase=ready status=done target=%q address=%s\n", startupTimestamp(), "api", u.apiServer.Address())
		u.startTelemetryReplay()
	}
	// Now that the API is up and status/dashboard reads no longer block on
	// it, run the broad retention sweep deferred above (#4373).
	// The completion channels are always closed, whether or not readiness was
	// reached, so shutdown never waits on a sweep that was never launched.
	u.startupRetentionSweepDone = startDeferredRetentionSweep(u.ctx, u.l, u.setup, u.retentionGate, u.worktreeRetentionErrors, u.readyNow)
	u.startupTelemetryRetentionSweepDone = startDeferredTelemetryRetentionSweep(
		u.ctx,
		u.l,
		u.setup,
		u.migrationBackupGaggles,
		u.telemetryRetentionConfig,
		u.telemetryRetentionGate,
		u.telemetryRetentionErrors,
		u.journalGenerationCleanupErrors,
		u.migrationBackupCleanupErrors,
		u.readyNow,
	)
	var terminalCleanupRetryCtx context.Context
	terminalCleanupRetryCtx, u.stopTerminalCleanupRetry = context.WithCancel(context.Background())
	defer u.stopTerminalCleanupRetry()
	u.terminalCleanupRetryDone = startTerminalCleanupRetry(terminalCleanupRetryCtx, u.cleanupRetries, u.terminalCleanupRetryErrors, u.readyNow)
	u.startupTerminalFinalize = startStartupTerminalFinalize(u.ctx, u.setup, u.resumeResult.Terminal)
	u.startupMergedPRCostSweepDone = u.mergedPRCostSweeps.startDeferred(u.ctx, u.readyNow)
	pf(u.stdout, "daemon started at %s (%d workflow(s)); API listening at %s://%s%s\n", u.root, len(u.setup.Entries), u.apiServer.Scheme(), u.apiServer.Address(), httpapi.Prefix)
	if u.webhookServer != nil {
		pf(u.stdout, "GitHub webhooks listening at http://%s%s\n", u.webhookServer.Address(), webhookhttp.Path)
	}
	if diagnosticsMode {
		pln(u.stdout, "diagnostics mode: ON — long-running stages get periodic process samples + lsof + un-truncated output recorded as run artifacts")
	}
	if u.fleetConnectorStarted {
		pln(u.stdout, "Fleet connector started")
	}
	// Notify-only release check (#4903). It runs off the critical path and
	// hands rendered text to the daemon loop below rather than writing to
	// stdout itself, so it adds no concurrent writer. It never applies an
	// update and never affects the daemon's health or exit status.
	var updatePendingState *daemonheartbeat.PendingUpdate
	u.updateNotices, u.updateCheckDone, updatePendingState = startUpdateCheck(u.ctx, u.root, u.setup.Config, u.stderr)
	u.templateNotices, u.templateChecksDone = startTemplateChecks(u.ctx, u.root)

	if !*u.quiet {
		tail, tailErr := journal.OpenInstanceLogTail(u.l.SchedulerDir())
		done := make(chan struct{})
		u.heartbeatDone = done
		go daemonheartbeat.Emit(u.ctx, u.stdout, u.l.SchedulerDir(), u.sched.WorkflowCount, tail, tailErr, heartbeatInterval, updatePendingState, done)
	}
	return u.supervise()
}

func (u *upSession) supervise() int {
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- u.sched.Run(u.ctx) }()

	u.schedulerFailed = false
	u.apiFailed = false
	u.webhookFailed = false
	u.configFailed = false
	configWatcherDone := false
	var webhookErrors <-chan error
	if u.webhookServer != nil {
		webhookErrors = u.webhookServer.Errors()
	}
daemonLoop:
	for {
		select {
		case update := <-u.updateNotices:
			update.report(u.stdout, u.stderr)
		case update := <-u.templateNotices:
			update.report(u.stdout, u.stderr)
			if u.reloader.readModel != nil {
				if err := u.reloader.readModel.PublishDefinitionsChanged(u.ctx); err != nil {
					pf(u.stderr, "warning: template status changed but portal invalidation failed: %v\n", err)
				}
			}
		case connectorErr := <-u.fleetConnectorDone:
			u.fleetConnectorDone = nil
			u.fleetConnectorStarted = false
			reportFleetConnectorStopped(u.stderr, connectorErr, u.ctx.Err())
		case u.runErr = <-schedulerDone:
			break daemonLoop
		case stopErr := <-u.supervisorStop:
			if stopErr != nil {
				u.schedulerFailed = true
				pf(u.stderr, "error: supervisor stop request: %v\n", stopErr)
			}
			u.stopDaemon()
			u.runErr = <-schedulerDone
			break daemonLoop
		case reloadErr := <-u.configDone:
			configWatcherDone = true
			if reloadErr == nil {
				reloadErr = errors.New("config watcher stopped unexpectedly")
			}
			if u.ctx.Err() == nil {
				u.configFailed = true
				pf(u.stderr, "error: config watcher stopped: %v\n", reloadErr)
			}
			u.stopDaemon()
			u.runErr = <-schedulerDone
			break daemonLoop
		case serveErr, ok := <-u.apiServer.Errors():
			u.apiFailed = true
			if !ok {
				serveErr = errors.New("server stopped unexpectedly")
			}
			pf(u.stderr, "error: HTTP API stopped: %v\n", serveErr)
			u.stopDaemon()
			u.runErr = <-schedulerDone
			break daemonLoop
		case serveErr, ok := <-webhookErrors:
			u.webhookFailed = true
			if !ok {
				serveErr = errors.New("server stopped unexpectedly")
			}
			pf(u.stderr, "error: webhook listener stopped: %v\n", serveErr)
			u.stopDaemon()
			u.runErr = <-schedulerDone
			break daemonLoop
		}
	}
	u.stopDaemon()
	if u.configLoopEnabled && !configWatcherDone {
		if reloadErr := <-u.configDone; reloadErr != nil {
			u.configFailed = true
			pf(u.stderr, "error: config watcher stopped: %v\n", reloadErr)
		}
	}
	u.openPRs.Stop()
	return u.joinWorkers()
}

func (u *upSession) joinWorkers() int {
	// Wait for both background goroutines to fully stop BEFORE any further
	// stdout/stderr writes below: each reacts to the same ctx cancellation
	// independently, so without this join a tick still in flight when
	// sched.Run returns would race the writes below on the shared io.Writer
	// (stdout/stderr are not safe for concurrent use).
	<-u.claimTickerDone
	<-u.sharedVisibilityDone
	<-u.stalledTickerDone
	<-u.updateCheckDone
	<-u.templateChecksDone
	<-u.telemetryRetentionTickerDone
	<-u.worktreeRetentionTickerDone
	<-u.storageHealthTickerDone
	<-u.recoveryInventoryTickerDone
	<-u.startupRetentionSweepDone
	<-u.startupTelemetryRetentionSweepDone
	<-u.mergedPRCostSweeps.tickerDone
	<-u.startupMergedPRCostSweepDone
	<-u.apiReadCacheLockSweepTickerDone
	<-u.delegationTickerDone
	<-u.cancelTickerDone
	<-u.applyTickerDone
	<-u.supervisorStopDone
	if u.heartbeatDone != nil {
		<-u.heartbeatDone
	}
	if u.fleetConnectorStarted && u.fleetConnectorDone != nil {
		select {
		case connectorErr := <-u.fleetConnectorDone:
			if connectorErr != nil &&
				!errors.Is(connectorErr, context.Canceled) &&
				!errors.Is(connectorErr, context.DeadlineExceeded) {
				pf(u.stderr, "warning: Fleet connector stopped: %v\n", connectorErr)
			}
		case <-time.After(5 * time.Second):
			pf(u.stderr, "warning: Fleet connector did not stop within 5s\n")
		}
	}

	if u.runErr != nil && !errors.Is(u.runErr, context.Canceled) && !errors.Is(u.runErr, context.DeadlineExceeded) {
		u.schedulerFailed = true
		pf(u.stderr, "error: scheduler stopped: %v\n", u.runErr)
	}

	return u.finish()
}

func (u *upSession) finish() int {
	drainResult := drainDaemonRuns(&u.wg, u.sched.Wait, u.setup.RunnerRegistry, *u.drainTimeout, u.force, u.stdout,
		func(active []trackedRun) []parkedRun { return parkedNonTerminalRuns(u.l, active) })
	u.stopClaimAdminSweep()
	u.stopTerminalCleanupRetry()
	u.startupTerminalFinalize.finishAfterDrain()
	<-u.terminalCleanupRetryDone
	runTerminalCleanupRetryFinal(u.cleanupRetries, u.terminalCleanupRetryErrors, u.readyNow)
	if !drainResult.forced {
		pln(u.stdout, "shutdown complete: all runs drained")
	} else {
		pf(u.stdout, "hard shutdown complete: %d run(s) stopped; they will resume from their last checkpoints on the next `goobers up`\n", drainResult.terminated)
	}

	// Keep the versioned API ready and discoverable while active runs drain:
	// stages may still need the daemon's claims plane to recover or renew
	// coordination state. Closing the listener, or flipping the recovery gate
	// first, makes those already-admitted runs fail even though this process is
	// deliberately waiting for them.
	u.ready.Store(false)
	shutdownErr, webhookShutdownErr := shutdownHTTPServers(u.apiServer, u.webhookServer, httpShutdownGrace)
	u.apiStopped = true
	if shutdownErr != nil {
		u.apiFailed = true
		pf(u.stderr, "error: %v\n", shutdownErr)
	}
	if webhookShutdownErr != nil {
		u.webhookFailed = true
		pf(u.stderr, "error: shut down webhook listener: %v\n", webhookShutdownErr)
	}
	if err := removeDaemonAPIAddress(u.apiAddressPath); err != nil {
		u.apiFailed = true
		pf(u.stderr, "error: %v\n", err)
	} else {
		u.apiAddressPublished = false
	}

	if u.apiFailed || u.webhookFailed || u.configFailed || u.schedulerFailed {
		return 1
	}
	u.stopDaemonHealth()
	if !drainResult.forced {
		if err := journalDaemonCleanShutdown(u.setup.InstanceLog, u.currentDaemon); err != nil {
			pf(u.stderr, "error: %v\n", err)
			return 1
		}
	}
	// Close telemetry, databases, watermarks, and the journal before the
	// command reports success: a lost final flush must be an exit-code
	// failure, not a silent clean shutdown (#3651).
	if u.shutdownSetup() != nil {
		return 1
	}
	return 0
}

type daemonDrainResult struct {
	forced     bool
	terminated int
}

func drainDaemonRuns(
	wg *sync.WaitGroup,
	waitScheduler func(),
	runners *daemonRunnerRegistry,
	timeout time.Duration,
	force <-chan struct{},
	stdout io.Writer,
	// listParked reports non-terminal runs the drain is NOT holding (#3453).
	// Nil disables the report, which keeps callers that have no layout — and
	// every existing test — unchanged.
	listParked func(active []trackedRun) []parkedRun,
) daemonDrainResult {
	done := make(chan struct{})
	go func() {
		// Admitted scheduler dispatches can still enter Start and add to wg.
		// Join those producers before waiting on the run counter; otherwise
		// an Add from zero can race with Wait (or arrive after it returned).
		waitScheduler()
		wg.Wait()
		close(done)
	}()

	// #3453: a gate-paused run is not held by the drain — Start returns on a
	// pause, releasing both the WaitGroup and the registry entry — so it is
	// invisible to ActiveRuns(). Reporting only the held population let the
	// drain print "no in-flight runs remain" while a non-terminal run sat
	// there. Naming them does not make the drain wait (since #3426 they are
	// recovered automatically at next boot via the pinned definition); it
	// stops "safe to restart" from being something the operator has to infer
	// from a message that is silent about what it cannot see.
	reportParked := func(prefix string, active []trackedRun) {
		if listParked == nil {
			return
		}
		parked := listParked(active)
		if len(parked) == 0 {
			return
		}
		ids := make([]string, len(parked))
		for i, run := range parked {
			ids[i] = run.Workflow + "/" + run.RunID
		}
		pf(stdout, "%s: %d run(s) parked at a gate and NOT held by this drain [%s]; "+
			"they are not waited for and resume at next boot\n",
			prefix, len(ids), strings.Join(ids, ", "))
	}
	printProgress := func(prefix string) {
		active := runners.ActiveRuns()
		ids := make([]string, len(active))
		for i, run := range active {
			ids[i] = run.Workflow + "/" + run.RunID
		}
		if len(ids) == 0 {
			pf(stdout, "%s: no in-flight runs remain; waiting for scheduler shutdown\n", prefix)
			reportParked(prefix, active)
			return
		}
		pf(stdout, "%s: %d run(s) remaining [%s]; send SIGINT/SIGTERM again to force shutdown\n",
			prefix, len(ids), strings.Join(ids, ", "))
		reportParked(prefix, active)
	}
	printProgress("shutting down: draining")

	progress := time.NewTicker(drainProgressInterval)
	defer progress.Stop()
	var timeoutC <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutC = timer.C
		defer timer.Stop()
	}

	for {
		select {
		case <-done:
			return daemonDrainResult{}
		case <-progress.C:
			printProgress("still draining")
		case <-timeoutC:
			return forceDaemonRuns(done, runners, stdout, fmt.Sprintf("drain timeout %s expired", timeout))
		case <-force:
			return forceDaemonRuns(done, runners, stdout, "repeated shutdown signal received")
		}
	}
}

func forceDaemonRuns(done <-chan struct{}, runners *daemonRunnerRegistry, stdout io.Writer, reason string) daemonDrainResult {
	terminated := runners.HardStopAll(func(count int) {
		pf(stdout, "hard shutdown: %s; terminating %d run(s) mid-stage; they will resume from their last checkpoints on the next `goobers up`\n",
			reason, count)
	})
	<-done
	return daemonDrainResult{forced: true, terminated: terminated}
}

// stalledSweepDependencies is the daemon-owned wiring the stalled-run sweep
// needs when it has to terminalize a run no live Runner owns.
// readSweepDowntime reads the graceful-drain downtime (#5601) and the host
// suspensions earlier daemon lifetimes journaled (#5891) that the stalled-run
// sweep credits, and starts observing new suspensions. The daemon has already
// journaled its own start, so the newest drain interval ends at this
// lifetime's beginning. A read failure credits nothing, which is the
// pre-#5601 behavior, and says so.
func readSweepDowntime(log *journal.InstanceLog, stderr io.Writer) ([]daemonDowntime, *hostsuspend.Ledger) {
	if log == nil {
		return nil, hostsuspend.NewLedger(nil, nil)
	}
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		pf(stderr, "warning: read daemon lifecycle for stalled-run downtime credit: %v\n", err)
		return nil, hostsuspend.NewLedger(log, nil)
	}
	return cleanDaemonDowntime(events), hostsuspend.NewLedger(log, hostsuspend.FromEvents(events))
}

func stalledSweepDependencies(setup *schedulerSetup, drainedDowntime []daemonDowntime, hostSuspensions *hostsuspend.Ledger, live *livejournal.Writer) *stalledSweepDeps {
	return &stalledSweepDeps{
		PrepareTerminal: func(runLayout instance.Layout) (runner.TerminalPreparer, error) {
			// The stalled run's gaggle is only knowable from its runs-tree
			// scope; cleanup must target that gaggle's own repo (#2692).
			project, err := terminalGaggleProject(runLayout)
			if err != nil {
				return nil, err
			}
			prepare, err := buildTerminalBranchPreparer(runLayout, setup.Config, project, setup.SharedRegistry, setup.SecretStores)
			if err != nil {
				return nil, err
			}
			return prepare.runnerPreparer(), nil
		},
		// The same observer the daemon's own runner carries (daemon.go's
		// runnerCfg.JournalAdvanced), so a run this sweep terminalizes reaches
		// the read model exactly as one that finishes under a live runner does.
		// Without it the terminal append records no intake watermark and the
		// projector never re-reads the run (#5278).
		JournalAdvancedContext: telemetryingest.RunIntakeObserverContext(setup.Watermarks, setup.InstanceLog),
		DrainedDowntime:        drainedDowntime,
		HostSuspensions:        hostSuspensions,
		CloseEngineRun:         closeTerminatedEngineRun(live),
	}
}

func newDaemonScheduler(setup *schedulerSetup, additionalOptions ...localscheduler.Option) *localscheduler.Scheduler {
	options := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(
		setup.RunConditions.MaxParallelRuns,
		setup.RunConditions.WorkflowBudgets,
		setup.RunConditions.WorkflowDailyBudgets,
	))
	// The refresher is nil when no workflow opts into MaxOpenPRs.
	if setup.OpenPRRefresher != nil {
		options = append(options, localscheduler.WithOpenPRCounter(setup.OpenPRRefresher))
	}
	if gate := daemonMemoryGate(setup.RunConditions); gate != nil {
		options = append(options, localscheduler.WithMemoryGate(gate))
	}
	options = append(options, additionalOptions...)
	return localscheduler.New(setup.Entries, setup.InstanceLog, options...)
}

// memoryHighWaterEnv names the environment variable that tunes the
// cgroup-aware admission gate (#3949). It is an environment variable rather
// than an instance.yaml field because it describes the container the daemon
// was given, not the instance's workflows: the same instance config is
// deployed to pods with different memory limits, and the operator who sets the
// limit is the one who knows the right threshold for it.
//
// Unset uses the built-in default. "off" (or "0") disables the gate entirely,
// which is the escape hatch for an operator who would rather take the OOM kill
// than the backpressure.
const memoryHighWaterEnv = "GOOBERS_MEMORY_HIGH_WATER"

// daemonMemoryGate builds the cgroup-aware admission gate, or nil if it is
// disabled. rc.MemoryHighWater is the instance-level default;
// GOOBERS_MEMORY_HIGH_WATER overrides it for the specific container/pod the
// daemon was actually given (#4218) — the same YAML-then-env layering used
// elsewhere in this package (ResolveEngineConfig, ResolveOTLPConfig).
// instance.RunConditions.ResolveMemoryHighWater does the actual parsing so
// `status` and the /api/v1/instance payload can report the same effective
// value without re-deriving these rules.
// buildStartupDaemonBehavior resolves the memory-gate and fsync settings
// (#4218), warns on stdout when fsync is disabled, and assembles the
// daemonBehavior published into up.lock. Pulled out of
// runUpContextWithForce to keep that function's cyclomatic complexity from
// re-accreting past its baseline.
func buildStartupDaemonBehavior(
	stdout io.Writer,
	runConditions instance.RunConditions,
	watchConfig, diagnostics, skipPreflight, disableReadModelReads bool,
	drainTimeout time.Duration,
) *daemonBehavior {
	resolvedMemoryHighWater, memoryGateDisabled, _ := runConditions.ResolveMemoryHighWater(os.LookupEnv)
	if warning := fsyncDisabledWarning(); warning != "" {
		pln(stdout, warning)
	}
	return &daemonBehavior{
		WatchConfig:           watchConfig,
		Diagnostics:           diagnostics,
		DrainTimeoutNanos:     int64(drainTimeout),
		SkipPreflight:         skipPreflight,
		DisableReadModelReads: disableReadModelReads,
		MemoryHighWater:       resolvedMemoryHighWater,
		MemoryGateDisabled:    memoryGateDisabled,
		FsyncDisabled:         journal.FsyncDisabled(),
	}
}

// fsyncDisabledWarning returns a startup warning when GOOBERS_DISABLE_FSYNC
// is set, or "" when it is not. Journal durability fsyncs skipped this way
// were previously invisible outside the daemon's own environment (#4218);
// this makes the daemon say so once, at the point an operator is most
// likely to be watching its stdout.
func fsyncDisabledWarning() string {
	if !journal.FsyncDisabled() {
		return ""
	}
	return "WARN: GOOBERS_DISABLE_FSYNC is set — journal durability fsyncs are skipped; a crash can lose the tail of an in-flight run"
}

func daemonMemoryGate(rc instance.RunConditions) localscheduler.MemoryGate {
	highWater, disabled, _ := rc.ResolveMemoryHighWater(os.LookupEnv)
	if disabled {
		return nil
	}
	return localscheduler.NewCgroupMemoryGate(highWater)
}

// newDaemonStorageGate builds tiered low-disk protection's gate for the
// filesystem containing root (#4873), resolving thresholds the same way
// `goobers status` and the Instance API report them — see
// instance.RunConditions.ResolveStorageThresholds — so every consumer agrees
// on the effective floors without re-deriving the defaulting rule.
func newDaemonStorageGate(root string, cfg *instance.Config) (*localscheduler.StorageGate, instance.StorageThresholds) {
	thresholds := cfg.RunConditions.ResolveStorageThresholds(cfg.Retention.RecoveryEffective())
	gate := localscheduler.NewStorageGate(root,
		thresholds.WarningFloorBytes, thresholds.WarningFloorPercent,
		thresholds.CriticalFloorBytes, thresholds.CriticalFloorPercent,
		thresholds.CriticalFloorDerived,
		thresholds.WarningFloorDerived,
	)
	return gate, thresholds
}

// storageHealthCode names the instance-journal EventError code tiered
// low-disk protection journals on a tier transition (#4873), reusing the
// generic error envelope every other best-effort diagnostic in this file
// does (see sweepErrorReporter) rather than introducing a new schema-
// registered event type for what is, functionally, one more operational
// signal alongside a sweep failure.
func storageHealthCode(tier localscheduler.StorageTier) string {
	switch tier {
	case localscheduler.StorageWarning:
		return "storage_health_warning"
	case localscheduler.StorageCritical:
		return "storage_health_critical"
	case localscheduler.StorageMeasurementUnavailable:
		return "storage_health_measurement_unavailable"
	default:
		return "storage_health_recovered"
	}
}

// startDaemonStorageHealth builds tiered low-disk protection's gate for
// setup.Root and takes its one-time startup sample (#4873: "measure ... at
// startup"), journaling and telemetering the result exactly like the
// periodic ticker started later will. Pulled out of runUpContextWithForce to
// keep that function's cyclomatic complexity and body length from
// re-accreting past its baseline (see startStorageHealthTicker, same
// reason).
func startDaemonStorageHealth(setup *schedulerSetup) (*localscheduler.StorageGate, instance.StorageThresholds) {
	gate, thresholds := newDaemonStorageGate(setup.Root, setup.Config)
	tier, changed := gate.Sample()
	reportStorageHealth(setup.InstanceLog, setup.Telemetry, gate, tier, changed)
	return gate, thresholds
}

// startStorageHealthTicker re-samples gate on interval for the life of the
// daemon (#4873's periodic half), journaling and telemetering only actual
// tier transitions. Pulled out of runUpContextWithForce alongside the other
// startTicker-shaped helpers in this file (see
// startAPIReadCacheLockSweepTicker) for the same complexity-budget reason as
// startDaemonStorageHealth.
func startStorageHealthTicker(ctx context.Context, setup *schedulerSetup, gate *localscheduler.StorageGate, interval time.Duration) <-chan struct{} {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tier, changed := gate.Sample()
				reportStorageHealth(setup.InstanceLog, setup.Telemetry, gate, tier, changed)
			}
		}
	}()
	return done
}

// reportStorageHealth journals a tier transition (deduplicated: the caller
// passes changed=false for every sample that didn't cross a boundary, and
// this is a no-op then) and always feeds the current reading to telemetry,
// satisfying #4873's "emit deduplicated status, log and telemetry signals" —
// status itself reads the gate directly (readservice.LocalSources.
// StorageHealthStats), so there is nothing else to update here.
func reportStorageHealth(log *journal.InstanceLog, tel *telemetry.Client, gate *localscheduler.StorageGate, tier localscheduler.StorageTier, changed bool) {
	stats := gate.Stats()
	tel.StorageHealthSampled(tier.String(), stats.FreeBytes, changed)
	if !changed {
		return
	}
	log.AppendBestEffort(journal.Event{
		Type: journal.EventError,
		Error: &journal.ErrorDetail{
			Code: storageHealthCode(tier),
			Message: fmt.Sprintf("storage health -> %s: %s free of %s total on %s",
				tier, memstat.FormatBytes(stats.FreeBytes), memstat.FormatBytes(stats.TotalBytes), stats.Path),
		},
	})
}

func stopReadServiceWorker(stop func() error, name string, stderr io.Writer) {
	if err := stop(); err != nil {
		pf(stderr, "error: stop %s: %v\n", name, err)
	}
}

func publishDaemonAPIAddress(path, address string) error {
	return daemonstate.PublishAPIAddress(path, address)
}

func removeDaemonAPIAddress(path string) error {
	return daemonstate.RemoveAPIAddress(path)
}

// worktreeRunTerminal answers worktree.ReapOptions.IsRunTerminal. Every call
// builds a fresh owner index, so callers call it once per Reap pass: the runs
// directory is listed at most once per pass (#6359) and the next pass still
// sees runs created since.
func worktreeRunTerminal(runsDir string) func(string) (bool, error) {
	owners := newRunOwnerIndex(runsDir)
	return func(worktreeID string) (bool, error) {
		phase, found, err := retainedWorktreePhase(owners, worktreeID, "")
		return found && terminalRunPhase(phase), err
	}
}

// buildPodVerifier selects the pod-token verifier. A configured key file gives
// stateless shared-key tokens, which is what a SPLIT deployment needs: the
// dispatcher runs inside `goobers worker`, so a token it mints must be
// verifiable by a different process. Unset keeps the daemon-local in-memory
// registry, correct whenever daemon and dispatcher share a process.
func buildPodVerifier(cfg *instance.Config) (podauth.Verifier, error) {
	path := strings.TrimSpace(cfg.API.PodTokenKeyFile)
	if path == "" {
		return podauth.NewRegistry(), nil
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pod token key %s: %w", path, err)
	}
	return podauth.NewSignedKey(bytes.TrimSpace(key))
}
