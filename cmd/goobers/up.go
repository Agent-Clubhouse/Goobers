package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/boundedagg"
	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/oidcauth"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/signals"
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

// delegationSweepInterval bounds how often runUpContext checks for delegated
// trigger requests (#343, rundelegate.go) from a `goobers run` invocation
// that found this daemon already holding up.lock. Deliberately much shorter
// than claimRecoverInterval — a human waiting on `goobers run` to return
// expects it to feel responsive, not lag behind a background maintenance
// cadence. Var, not const, so tests can shrink it further.
var delegationSweepInterval = 2 * time.Second

// heartbeatInterval is a var so daemon tests do not wait a full minute.
var heartbeatInterval = time.Minute

const sweepErrorReportEvery = 12

var httpShutdownGrace = 5 * time.Second

const daemonAPIAddressFileName = "api.address"

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

type sweepErrorReporter struct {
	log         *journal.InstanceLog
	code        string
	lastMessage string
	consecutive int
	reportEvery int
}

func newSweepErrorReporter(log *journal.InstanceLog, code string) *sweepErrorReporter {
	return &sweepErrorReporter{log: log, code: code, reportEvery: sweepErrorReportEvery}
}

func (r *sweepErrorReporter) report(err error) {
	if err == nil {
		r.lastMessage = ""
		r.consecutive = 0
		return
	}
	// Bound the persisted message regardless of how many entries a sweep
	// aggregated: this is the single write-boundary choke point guarding every
	// sweep reporter (stalled/trigger/claim/cancel/telemetry-retention) so a
	// giant record can never reach the scheduler journal and bloat the store
	// (#1414). Source-level bounding (boundedagg.Join in the stalled-run sweep)
	// is belt-and-suspenders on top of this.
	message := boundedagg.Bound(err.Error(), boundedagg.DefaultMaxBytes)
	if message != r.lastMessage {
		r.lastMessage = message
		r.consecutive = 1
	} else {
		r.consecutive++
	}
	if r.consecutive != 1 && (r.consecutive-1)%r.reportEvery != 0 {
		return
	}
	_ = r.log.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: r.code, Message: message},
		Runner: map[string]any{
			"consecutiveFailures": r.consecutive,
		},
	})
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
	"Legacy spans-only run directories are reported as cleanup candidates\n" +
	"and preserved by default. --cleanup-spans-only-runs deletes them at\n" +
	"startup after reporting each candidate.\n\n" +
	"Startup validates the resolved instance config and refuses to run on\n" +
	"errors. --skip-preflight bypasses that refusal with a prominent warning.\n\n" +
	"A Git workflowSource continuously reconciles its tracked ref. Local Git\n" +
	"ref changes wake the loop immediately; periodic fetch-and-compare polling\n" +
	"is always active, and authenticated GitHub push deliveries wake it when\n" +
	"webhook.secret is configured. Invalid revisions are rejected with the\n" +
	"last-known-good definitions left running. --watch-config separately watches\n" +
	"direct edits to the materialized config directory.\n\n" +
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
	"selects a process-local development watcher, --diagnostics is temporary\n" +
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

func runUpContextWithForce(parentCtx context.Context, force <-chan struct{}, args []string, stdout, stderr io.Writer) int {
	webhookGate, err := webhookhttp.NewDispatchGate(parentCtx)
	if err != nil {
		pf(stderr, "error: initialize daemon lifecycle: %v\n", err)
		return 1
	}
	ctx := webhookGate.Context()
	var ready atomic.Bool
	// Named subsystem readiness checks (#3806), surfaced on /readyz alongside
	// the overall Ready gate above. Each flips exactly once, in startup
	// order, as its own phase below completes; none of them gate anything —
	// they are purely additive instrumentation describing WHY the daemon
	// isn't ready yet. `ready` above remains the single source of truth both
	// /api/v1/health.Ready and /readyz.Ready read, so the two surfaces can
	// never disagree — by the time `ready` flips true (webhookGate.Start(),
	// below), every one of these four has already flipped true too, in this
	// same, sequential, error-returns-early function body.
	var (
		configLoaded    atomic.Bool  // instance config + scheduler wiring validated
		stateOpen       atomic.Bool  // scheduler's run-tracking state reconciled from disk
		resumeComplete  atomic.Bool  // crash-resume of interrupted runs finished
		sweepsStarted   atomic.Bool  // initial sweeps ran once and their periodic tickers are live
		schedulerTicked atomic.Bool  // scheduler's heartbeat ticked at least once (liveness grace)
		lastTickAtNanos atomic.Int64 // in-memory heartbeat /healthz reads (#3806); unix nanos
	)
	stopDaemon := func() {
		ready.Store(false)
		webhookGate.Stop()
	}
	parentBridgeDone := make(chan struct{})
	go func() {
		defer close(parentBridgeDone)
		select {
		case <-parentCtx.Done():
			stopDaemon()
		case <-ctx.Done():
		}
	}()
	defer func() {
		stopDaemon()
		<-parentBridgeDone
	}()

	fs := newCLIFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "up")
	quiet := fs.Bool("quiet", false, "suppress periodic liveness heartbeats")
	diagnostics := fs.Bool("diagnostics", false, "capture deep per-stage diagnostics (process samples, lsof, un-truncated output) for hang debugging")
	watchConfig := fs.Bool("watch-config", false, "hot-reload edits to the materialized config directory (Git workflow sources reconcile automatically)")
	drainTimeout := fs.Duration("drain-timeout", 0, "force shutdown if graceful drain exceeds this duration (default: wait indefinitely)")
	var notifications notifyFlag
	fs.Var(&notifications, "notify", "send desktop notifications for escalated and failed runs; use --notify=all for every terminal outcome")
	skipPreflight := fs.Bool("skip-preflight", false, "start despite instance config validation errors (unsafe)")
	cleanupSpansOnlyRuns := fs.Bool("cleanup-spans-only-runs", false, "delete reported legacy spans-only run directories at startup")
	disableReadModelReads := fs.Bool("disable-read-model-reads", false, "design §6.6 rollback: force authoritative journal scans for this run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *drainTimeout < 0 {
		pf(stderr, "error: --drain-timeout must not be negative\n")
		return 2
	}
	diagnosticsMode = *diagnostics
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	// The shipped image runs the daemon as its container's pid 1, which makes
	// it the kernel's reparent target for every stage descendant that outlives
	// its parent — and a Go program waits for nothing but its own exec.Cmd
	// children, so those descendants would stay zombies for the life of the pod
	// (#3398). Install the missing init half before any stage can start. It is
	// pid-1-guarded, so a local `goobers up` is untouched and stays silent.
	if proc.StartOrphanReaper(ctx) {
		pf(stdout, "startup: running as container init (pid 1); reaping orphaned stage descendants\n")
	}

	l := instance.NewLayout(root)
	pf(stdout, "startup: validating instance configuration\n")
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	if code := runStartupConfigPreflight(root, *skipPreflight, stderr); code != 0 {
		return code
	}
	pf(stdout, "startup: instance configuration valid\n")
	startupConfig, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: invalid instance.yaml: %v\n", err)
		return 1
	}
	if warning := windowsLargeRepoEnvironmentWarning(startupConfig, l.WorkcopiesDir(), realWindowsLargeRepoPreflightDeps()); warning != "" {
		pln(stdout, warning)
	}
	livenessTimeout, err := startupConfig.Runner.LivenessTimeoutDuration()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	// Single-instance lock (#23 AC3): a second `up` on the same instance root
	// must fail fast with a clear message, not silently race the first.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	lockPath := filepath.Join(l.SchedulerDir(), "up.lock")
	priorLock, err := readPriorDaemonLock(lockPath)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	release, err := acquireDaemonLock(lockPath, root, livenessTimeout, &daemonBehavior{
		WatchConfig:           *watchConfig,
		Diagnostics:           *diagnostics,
		DrainTimeoutNanos:     int64(*drainTimeout),
		SkipPreflight:         *skipPreflight,
		DisableReadModelReads: *disableReadModelReads,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer release()
	currentDaemon, err := readCurrentDaemonIdentity(lockPath)
	if err != nil {
		pf(stderr, "error: read current daemon lock: %v\n", err)
		return 1
	}
	apiAddressPath := filepath.Join(l.SchedulerDir(), daemonAPIAddressFileName)
	if err := removeDaemonAPIAddress(apiAddressPath); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	var wg sync.WaitGroup
	var setup *schedulerSetup
	// DS6 (distributed-state-and-coordination.md §10): the gate keeps every
	// expired-claim reap — setup's included — a no-op until the renewal set
	// has been rebuilt from ledger + liveness below.
	claimRecoveryGate := localscheduler.NewRecoveryGate()
	setupOptions := []schedulerSetupOption{
		withDesktopNotifications(notifications, stderr),
		withStartupProgress(func(message string) {
			pf(stdout, "startup: %s\n", message)
		}),
		withClaimRecoveryGate(claimRecoveryGate),
	}
	if *skipPreflight {
		setup, err = buildSchedulerSetupAllowingInvalidConfig(ctx, l, &wg, setupOptions...)
	} else {
		setup, err = buildSchedulerSetup(ctx, l, &wg, setupOptions...)
	}
	if err != nil {
		printValidationIssues(stderr, validationReportFromError(err))
		pf(stderr, "error: initialize daemon scheduler: %v\n", err)
		return 1
	}
	pf(stdout, "startup: scheduler initialized\n")
	// #3806: instance config validated, definitions/scheduler wiring built.
	configLoaded.Store(true)
	defer setup.Shutdown(context.Background())
	if err := journalDaemonStart(setup.InstanceLog, priorLock, currentDaemon); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := journalValidationWarnings(setup.InstanceLog, setup.Validation.Warnings()); err != nil {
		pf(stderr, "error: %v\n", err)
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
	blobStore, err := blobstore.NewDir(l.BlobStoreDir())
	if err != nil {
		pf(stderr, "error: initialize blob store: %v\n", err)
		return 1
	}
	// The live journal writer (DS4) authors engine-run journals from events
	// emitted as they happen; the projection reconciler below is thereby the
	// repair/verify path (DS5), never the authority, for live-authored runs.
	liveJournals, err := newLiveJournalWriter(l, setup.Config, setup.Definitions, setup.Watermarks, setup.InstanceLog, blobStore)
	if err != nil {
		pf(stderr, "error: initialize live journal writer: %v\n", err)
		return 1
	}
	if liveJournals != nil {
		defer liveJournals.Close()
	}
	// The SAME store the writer adopts spans from: DS5 verifies a
	// live-authored journal against a re-projection, so a source given to one
	// and not the other turns every adopted span into a false divergence.
	stopEngineProjection, err := startEngineProjection(ctx, l, setup.Config, setup.Definitions, setup.Watermarks, setup.InstanceLog, setup.Telemetry, liveJournals, blobStore)
	if err != nil {
		pf(stderr, "error: start engine projection reconciler: %v\n", err)
		return 1
	}
	defer stopEngineProjection()
	printValidationWarnings(stdout, setup.Validation.CLIWarnings())
	if warning := webhookConfigurationWarning(setup.Definitions, setup.Config); warning != "" {
		pln(stdout, warning)
	}
	if err := handleSpansOnlyRunCleanup(l, *cleanupSpansOnlyRuns, stdout); err != nil {
		pf(stderr, "error: clean up spans-only run directories: %v\n", err)
		return 1
	}

	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      l,
		Config:      setup.Config,
		Definitions: setup.Definitions,
		Validation:  setup.Validation,
		Telemetry:   setup.RollupDB,
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
		ReadModel:      setup.ReadModel,
		RetentionStats: setup.RetentionStats,
		WorkItemLookup: statusWorkItemLookup(l.Root, setup.Definitions),
		SchedulerHeartbeat: func() (time.Time, error) {
			return daemonstate.Read(lockPath)
		},
		LivenessTimeout: livenessTimeout,
	}, ready.Load)
	if err != nil {
		pf(stderr, "error: initialize read service: %v\n", err)
		return 1
	}
	if *disableReadModelReads {
		// The design §6.6 rollback, made operator-reachable (#2036):
		// DisableReadModelReads previously had no caller anywhere, so the
		// documented "a flag flip, never a deploy" rollback did not exist in
		// practice. read.db itself is untouched — this only forces every list
		// request back onto authoritative journal scans for this run.
		reads.DisableReadModelReads()
	}
	// Move the active-run count off the request path (#1741). Six read routes
	// used to walk every run directory in history and open every journal, per
	// request — 17.2 s cold on the live instance to answer "2" (design §2.1).
	// The daemon is the only construction that is long-lived enough for a
	// background sample to be warm, so it is the only one that starts it.
	stopActiveSampler := reads.StartActiveRunSampler(0)
	defer func() {
		if err := stopActiveSampler(); err != nil {
			pf(stderr, "error: stop active-run sampler: %v\n", err)
		}
	}()
	apiLog := log.New(stderr, "http API: ", log.LstdFlags)
	// Unconfigured instances keep the tier-1 posture verbatim: null
	// authenticator, allow-all authorizer, plain HTTP on loopback. api.auth
	// swaps in the OIDC authenticator plus the role-floor authorizer, and
	// api.tls upgrades the transport (#640/#644).
	apiAuthorizer := httpapi.AllowAll
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
	var apiHandlerOpts []httpapi.HandlerOption
	if setup.ReadModel != nil {
		apiHandlerOpts = append(apiHandlerOpts, httpapi.WithChangeFeedStream(setup.ReadModel))
	}
	interventions := newRunInterventionService(l, setup, &wg, apiLog)
	// The write planes (#3509, distributed-state-and-coordination.md §7):
	// claims over the same ledger + flock the CLI claimants use, triggers
	// through the same scheduler path the pending-triggers sweep dispatches,
	// HITL resolution over the intervention machinery. The file seams remain
	// for local/mode-1 callers.
	triggerPlane := newDaemonTriggerService()
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
	credentialPlane := newDaemonCredentialService(l, setup.Config, setup.SecretStores, setup.SharedRegistry, setup.InstanceLog)
	credentialPlane.Replace(credentialPlaneDefinitionsFromSet(setup.Definitions))
	setup.CredentialPlane = credentialPlane
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
	surrenderStore, err := dispatcher.NewSurrenderDir(filepath.Join(l.BlobStoreDir(), "surrender"))
	if err != nil {
		pf(stderr, "error: initialize surrender plane: %v\n", err)
		return 1
	}
	apiHandlerOpts = append(apiHandlerOpts,
		httpapi.WithInterventions(interventions),
		httpapi.WithInterventionContext(ctx),
		httpapi.WithClaimService(newDaemonClaimService(l, setup.InstanceLog)),
		httpapi.WithTriggerService(triggerPlane),
		httpapi.WithEscalationService(newEscalationResolutionAdapter(interventions)),
		httpapi.WithCredentialService(credentialPlane),
		httpapi.WithBlobService(blobStore),
		httpapi.WithSurrenderService(surrenderStore),
	)
	if liveJournals != nil {
		// The journal plane (§8): remote stage pods emit their run's journal
		// events here; the daemon's own in-process emitters use the writer
		// directly and never pass through HTTP.
		apiHandlerOpts = append(apiHandlerOpts, httpapi.WithJournalService(liveJournals))
	}
	if instance.IsLoopbackListenAddress(apiListenAddress(setup.Config)) {
		apiHandlerOpts = append(apiHandlerOpts, httpapi.WithRunRevealer(runDirectoryRevealer(l)))
	}
	// Pod-plane verifier: shared-key when configured (split daemon/dispatcher
	// deployments — Goobers#3701), else the daemon-local in-memory registry.
	podVerifier, perr := buildPodVerifier(setup.Config)
	if perr != nil {
		pf(stderr, "error: initialize pod token verifier: %v\n", perr)
		return 1
	}
	if auth := setup.Config.API.Auth; auth != nil && auth.OIDC != nil {
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
			pf(stderr, "error: initialize HTTP API authenticator: %v\n", err)
			return 1
		}
		// Pod-to-daemon authn (#3509 §14 open point, resolved as per-run
		// minted bearers): chain the pod-token verifier in front of the human
		// OIDC authenticator. The registry is daemon-local (sound under DS1);
		// the mode-3 dispatcher mints into it at stage dispatch (#3482).
		chained, err := podauth.NewAuthenticator(podVerifier, authenticator)
		if err != nil {
			pf(stderr, "error: initialize HTTP API authenticator: %v\n", err)
			return 1
		}
		apiHandlerOpts = append(apiHandlerOpts, httpapi.WithAuthenticator(chained))
		apiAuthorizer = httpapi.RequireRoles()
	} else if !instance.IsLoopbackListenAddress(apiListenAddress(setup.Config)) {
		// Non-loopback with no human authenticator configured: serve the pod
		// plane only, denying every non-pod request. This satisfies SEC-043's
		// requirement for a REAL authenticator without forcing an operator who
		// wants no human surface to stand up an OIDC issuer to get one
		// (Goobers#3701). It never admits an unauthenticated request — the
		// fallback denies rather than allowing.
		chained, err := podauth.NewAuthenticator(podVerifier, httpapi.DenyAllAuthenticator{})
		if err != nil {
			pf(stderr, "error: initialize HTTP API authenticator: %v\n", err)
			return 1
		}
		apiHandlerOpts = append(apiHandlerOpts, httpapi.WithAuthenticator(chained))
		apiAuthorizer = httpapi.RequireRoles()
	}
	handler, err := httpapi.NewHandler(reads, apiAuthorizer, apiLog, apiHandlerOpts...)
	if err != nil {
		pf(stderr, "error: initialize HTTP API: %v\n", err)
		return 1
	}
	// #3806: /healthz and /readyz are registered OUTSIDE the versioned router
	// httpapi.NewHandler just built — no authenticate/authorize/admission/
	// budget — so a kubelet probe reaches them with no credential regardless
	// of api.auth (including this daemon's own DenyAllAuthenticator fallback
	// for a non-loopback bind with no human authenticator configured, just
	// above). Every other path keeps going through the versioned handler
	// exactly as before; WrapWithProbes forwards authenticatedTransport() and
	// shutdown() straight through so NewServer's SEC-043 gate below and
	// apiHandler's own SSE-close lifecycle both keep working unchanged.
	//
	// The checks themselves live on daemonProbeState (daemon_probes.go) —
	// a named type, not two inline closures — so they are directly unit
	// testable without a real daemon.
	probes := &daemonProbeState{
		ready:           &ready,
		configLoaded:    &configLoaded,
		stateOpen:       &stateOpen,
		resumeComplete:  &resumeComplete,
		sweepsStarted:   &sweepsStarted,
		schedulerTicked: &schedulerTicked,
		lastTickAtNanos: &lastTickAtNanos,
		livenessTimeout: livenessTimeout,
		now:             time.Now,
	}
	handler = httpapi.WrapWithProbes(handler, probes.liveness, probes.readiness)
	var apiServerOpts []httpapi.ServerOption
	if tlsConfig := setup.Config.API.TLS; tlsConfig != nil {
		apiServerOpts = append(apiServerOpts, httpapi.WithTLS(tlsConfig.CertFile, tlsConfig.KeyFile))
	}
	apiServer, err := httpapi.NewServer(apiListenAddress(setup.Config), handler, apiLog, apiServerOpts...)
	if err != nil {
		pf(stderr, "error: initialize HTTP API: %v\n", err)
		return 1
	}
	// Rebuild the claim-renewal set from the LEDGER plus run liveness before
	// any reap is permitted — DS6's load-bearing ordering
	// (distributed-state-and-coordination.md §10): this process's in-memory
	// run tracking is empty right now, but a distributed run dispatched by
	// the previous daemon process is still executing on the engine, and its
	// claims must be renewed — not reaped — across the restart. Only a
	// renewal pass whose ledger write completed opens the gate; a failed pass
	// leaves it closed and the periodic tick below retries both halves.
	claimLiveness, closeClaimLiveness, err := buildClaimLivenessProbe(setup.Config, setup.RunnerRegistry.RunIDs)
	if err != nil {
		pf(stderr, "error: build claim liveness probe: %v\n", err)
		return 1
	}
	defer closeClaimLiveness()
	if probeErr, renewErr := rebuildClaimRenewalSet(ctx, l, claimLiveness, claimRecoveryGate); renewErr != nil {
		if !isJournaledClaimsLockTimeout(renewErr) {
			pf(stdout, "warning: rebuild claim renewal set: %v\n", renewErr)
		}
	} else if probeErr != nil {
		pf(stdout, "warning: claim liveness probe degraded (renewed fail-live): %v\n", probeErr)
	}

	// Claim recovery (#131/#793): released once now and periodically thereafter
	// to recover expired leases and claim cleanup deferred by a terminal
	// finalizer's bounded lock timeout — before the scheduler starts admitting
	// new ticks, same ordering rationale as crash-resume below. withClaimLock
	// serializes this against a concurrent
	// `goobers backlog-query` subprocess claiming/releasing on the same
	// ledger file (providercmd.go's doc). recoverExpiredClaims itself never
	// touches stdout/stderr — it returns the released entries so ONLY the
	// synchronous startup call site below prints; the periodic goroutine
	// below deliberately does not (see its own comment).
	recoverExpiredClaims := func(now time.Time) ([]localscheduler.ClaimEntry, error) {
		return recoverClaims(l, setup.InstanceLog, now, interventions.interventionActive, claimRecoveryGate)
	}
	startupReleased := append([]localscheduler.ClaimEntry(nil), setup.RecoveredClaims...)
	newlyReleased, err := recoverExpiredClaims(time.Now())
	if err != nil && !isJournaledClaimsLockTimeout(err) {
		pf(stderr, "error: recover expired claims: %v\n", err)
		return 1
	}
	startupReleased = append(startupReleased, newlyReleased...)
	for _, entry := range startupReleased {
		pf(stdout, "recovered expired claim %s (was held by run %s)\n", entry.ItemID, entry.RunID)
	}

	// Scratch workspaces have no git metadata to recover. Once this daemon
	// holds the instance lock, every stage-* entry belongs to the prior process
	// and can be removed before interrupted runs allocate fresh workspaces.
	for gaggle, manager := range setup.WorktreesByGaggle {
		if err := runner.ReapScratchWorkspaces(filepath.Join(manager.Root, "scratch")); err != nil {
			pf(stderr, "error: reap scratch workspaces for gaggle %s: %v\n", gaggle, err)
			return 1
		}
	}
	if setup.LegacyWorktrees != nil {
		if err := runner.ReapScratchWorkspaces(filepath.Join(setup.LegacyWorktrees.Root, "scratch")); err != nil {
			pf(stderr, "error: reap legacy scratch workspaces: %v\n", err)
			return 1
		}
	}

	// Reap crash-orphaned worktrees before anything tries to resume into one
	// of their keys (issue #136): a mid-stage crash otherwise leaves a
	// worktree directory that makes worktree.Create refuse forever (fixed
	// separately by adopt-and-reset, but Reap is still what actually reclaims
	// the disk space and the git worktree-list registration).
	for gaggle, manager := range setup.WorktreesByGaggle {
		if _, warnings, err := manager.Reap(ctx, worktree.ReapOptions{
			IsRunTerminal: worktreeRunTerminal(l.ForGaggle(gaggle).RunsDir()),
		}); err != nil {
			pf(stderr, "error: reap worktrees for gaggle %s: %v\n", gaggle, err)
			return 1
		} else {
			for _, w := range warnings {
				pf(stdout, "warning: skipped worktree cleanup %s: %v\n", w.Path, w.Err)
			}
		}
	}
	if setup.LegacyWorktrees != nil {
		if _, warnings, err := setup.LegacyWorktrees.Reap(ctx, worktree.ReapOptions{
			IsRunTerminal: worktreeRunTerminal(l.RunsDir()),
		}); err != nil {
			pf(stderr, "error: reap legacy worktrees: %v\n", err)
			return 1
		} else {
			for _, w := range warnings {
				pf(stdout, "warning: skipped worktree cleanup %s: %v\n", w.Path, w.Err)
			}
		}
	}
	if err := pruneConfiguredRetention(ctx, l, setup, stdout, stderr); err != nil {
		pf(stderr, "error: prune retained worktrees and branches: %v\n", err)
		return 1
	}
	telemetryRetentionConfig := instance.TelemetryRetentionConfig{}
	if setup.Config.Telemetry.Retention != nil {
		telemetryRetentionConfig = *setup.Config.Telemetry.Retention
	}
	telemetryPruned, err := pruneConfiguredTelemetryRetention(l, telemetryRetentionConfig, setup.RollupDB, time.Now())
	if err != nil {
		pf(stderr, "error: prune retained telemetry: %v\n", err)
		return 1
	}
	for _, result := range telemetryPruned {
		pf(stdout, "telemetry pruned run=%q reason=%s\n", result.RunID, result.Reason)
	}

	// Prune crash-abandoned orphan runs and run-creation staging directories
	// before anything else touches the runs tree (#2035): a mid-Create crash's
	// os.RemoveAll cleanup is in-process only, so a `.runs.creating` residue
	// otherwise sits until an operator happens to run `goobers telemetry
	// prune-orphans` — the same gap worktree Reap (above) and telemetry
	// retention (immediately above) already close for their own trees.
	orphansPruned, err := pruneOrphansAtStartup(l, time.Now())
	if err != nil {
		pf(stderr, "error: prune orphan run directories: %v\n", err)
		return 1
	}
	for _, result := range orphansPruned {
		source := "run"
		if result.CreationStage {
			source = "creation-stage"
		}
		pf(stdout, "pruned orphan run directory name=%q source=%s path=%q lastModified=%s\n",
			result.Name, source, result.RunDir, result.LastModified.UTC().Format(time.RFC3339))
	}

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
		lastTickAtNanos.Store(tickAt.UnixNano())
		// #3806: /healthz's liveness grace ends once the scheduler has ticked
		// at least once — set regardless of whether the on-disk heartbeat
		// write below succeeds, since the tick itself (not that write) is
		// what "has the main loop reached its steady-state loop" means.
		schedulerTicked.Store(true)
	}
	sched := newDaemonScheduler(setup,
		localscheduler.WithTickHeartbeat(livenessTimeout/2, func(tickAt time.Time) error {
			err := daemonstate.Refresh(lockPath, tickAt)
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
	)
	sourceReconcileWake := make(chan struct{}, 1)
	wakeSourceReconcile := func(context.Context) {
		select {
		case sourceReconcileWake <- struct{}{}:
		default:
		}
	}
	interventions.AttachScheduler(sched)
	triggerPlane.AttachScheduler(sched)
	webhookLog := log.New(stderr, "webhook: ", log.LstdFlags)
	webhookServer, err := buildWebhookServer(ctx, setup, sched, webhookGate, webhookLog, wakeSourceReconcile)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	runDirs, err := l.RunDirs()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := sched.ReconcileAll(runDirs, time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// #3806: the scheduler's run-tracking state has been reconciled from the
	// run directories already on disk.
	stateOpen.Store(true)
	stalledRunTimeout, err := setup.RunConditions.StalledRunTimeoutDuration()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	maxRunDuration, err := setup.RunConditions.MaxRunDurationDuration()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	stalledSweepErrors := newSweepErrorReporter(setup.InstanceLog, "stalled_run_sweep_failed")
	sweepStalled := func(now time.Time) error {
		return sweepStalledRuns(
			l,
			setup.RunnerRegistry,
			setup.LegacyRunner,
			setup.InstanceLog,
			func(runLayout instance.Layout) (runner.TerminalPreparer, error) {
				// The stalled run's gaggle is only knowable from its runs-tree
				// scope; cleanup must target that gaggle's own repo (#2692).
				project, err := terminalGaggleProject(runLayout)
				if err != nil {
					return nil, err
				}
				return buildTerminalBranchPreparer(runLayout, setup.Config, project, setup.SharedRegistry, setup.SecretStores)
			},
			setup.TerminalNotifier,
			sched.ReleaseRun,
			now,
			stalledRunTimeout,
			maxRunDuration,
		)
	}
	// Reap stale journals before crash-resume can refresh them with a new
	// stage heartbeat.
	stalledSweepErrors.report(sweepStalled(time.Now()))

	if err := apiServer.Start(); err != nil {
		pf(stderr, "error: start HTTP API: %v\n", err)
		return 1
	}
	if webhookServer != nil {
		if err := webhookServer.Start(); err != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
			_ = apiServer.Shutdown(shutdownCtx)
			shutdownCancel()
			pf(stderr, "error: start webhook listener: %v\n", err)
			return 1
		}
	}
	apiStopped := false
	defer func() {
		if apiStopped {
			return
		}
		stopDaemon()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
		defer shutdownCancel()
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			pf(stderr, "error: %v\n", err)
		}
		if webhookServer != nil {
			if err := webhookServer.Shutdown(shutdownCtx); err != nil {
				pf(stderr, "error: shut down webhook listener: %v\n", err)
			}
		}
	}()

	openPRs := newOpenPRLoop(ctx, setup.OpenPRRefresher)
	defer openPRs.Stop()

	// The reloader is always constructed (not gated behind --watch-config)
	// because `goobers apply` (#459) needs to run exactly one reload check
	// on demand regardless of whether continuous watching is enabled — the
	// flag only decides whether its own ticker loop (wired further below)
	// runs automatically.
	reloader := &configReloader{
		layout:         l,
		setup:          setup,
		scheduler:      sched,
		openPRs:        openPRs,
		reads:          reads,
		readModel:      setup.ReadModel,
		wg:             &wg,
		appliedDigest:  setup.ConfigDigest,
		observedDigest: setup.ConfigDigest,
	}

	// Crash-resume: any run left non-terminal by a prior crash or unclean
	// shutdown restarts now, before the scheduler starts admitting new ticks
	// (#23 AC: restart via Runner.Resume). A run whose workflow no longer
	// resolves in config is skipped with a warning (issue #135), not fatal —
	// recover it with `goobers run abort <run-id>`. Each resumed run also
	// incrementally ingests into the telemetry rollup once its outcome is
	// known (issue #127).
	resumed, warned, err := resumeInterruptedRunsWithRunners(ctx, l, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, setup.Watermarks, sched.ReleaseReconciled, &wg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	for _, runID := range resumed {
		pf(stdout, "resuming interrupted run %s\n", runID)
	}
	// Renew resumed runs' claims immediately rather than waiting up to
	// claimRecoverInterval for the first periodic tick (#2014): the startup
	// recovery sweep above already ran BEFORE resume tracked anything, on the
	// prior process's now possibly-stale leases, so a resumed run's claim
	// could otherwise sit unrenewed — and so reapable — for most of a sweep
	// interval right when a restart just made that most likely. The resumed
	// runs are tracked by the registry now, so the ledger-driven pass covers
	// exactly them (plus any engine-live holders — idempotent). Best-effort,
	// same as the periodic sweep: a renewal failure here does not fail daemon
	// start, since the claim ledger's own reap is what it would fail open to.
	if len(resumed) > 0 {
		if _, _, err := renewLiveClaims(ctx, l, claimLiveness, DefaultClaimLease); err != nil && !isJournaledClaimsLockTimeout(err) {
			pf(stdout, "warning: renew resumed claims: %v\n", err)
		}
	}
	for _, runID := range warned {
		pf(stdout, "warning: run %s references a workflow no longer in config — skipped; recover with `goobers run abort %s`\n", runID, runID)
	}
	// #3806: crash-resume of every interrupted run finished (this is the
	// startup phase whose duration is unbounded and scales with interrupted-
	// run count, so a kubelet startupProbe against /readyz must wait this
	// out with a generous failureThreshold, not a short initialDelay).
	resumeComplete.Store(true)

	// Sweep once before announcing readiness so requests and responses orphaned
	// across daemon lifetimes are handled without waiting for the first tick.
	triggerSweepErrors := newSweepErrorReporter(setup.InstanceLog, "trigger_sweep_failed")
	triggerSweepErrors.report(sweepPendingTriggers(ctx, l.SchedulerDir(), sched, time.Now))
	claimAdminSweepErrors := newSweepErrorReporter(setup.InstanceLog, "claim_admin_sweep_failed")
	claimAdminSweepErrors.report(sweepPendingClaimAdminRequests(l.SchedulerDir(), setup.InstanceLog, time.Now))
	// #831's daemon-side half: cancel one live in-flight run on operator request
	// by resolving its owning Runner and calling CancelRun. Its own ticker (below)
	// keeps a worst-case wedged-stage cancellation — which blocks in CancelRun for
	// the cancellation + terminalization grace — from stalling the trigger/claim
	// sweeps that share the delegation ticker.
	cancelSweepErrors := newSweepErrorReporter(setup.InstanceLog, "cancel_sweep_failed")
	cancelSweep := func() error {
		return sweepPendingCancelRequests(l.SchedulerDir(), setup.RunnerRegistry, setup.InstanceLog, sched.ReleaseRun, time.Now)
	}
	cancelSweepErrors.report(cancelSweep())

	// #459's daemon-side half: on operator request (`goobers apply`), run
	// exactly one config-reload check now instead of waiting for
	// --watch-config's own ticker (or performing one at all if it's off). For
	// a git-tracked workflowSource, first pull the tracked ref's latest
	// commit into the config directory — the same validate-or-keep-LKG
	// contract reloader.pollOnce already enforces for a hand-edited file
	// applies unchanged to a git-sourced one.
	applySweepErrors := newSweepErrorReporter(setup.InstanceLog, "apply_sweep_failed")
	// #3274: a github-app workflowSource mints installation tokens instead of
	// reading a static token ref. The minter is built once here — the
	// composition root, since internal/instance cannot import
	// internal/githubapp — and shared by the apply sweep and the reconcile
	// loop below, so both draw on one near-expiry-refreshing token cache.
	var workflowSourceAppTokens instance.GitTokenSource
	if source := setup.Config.WorkflowSource; source != nil && source.GitHubAppAuth() {
		minted, mintErr := newWorkflowSourceAppTokenSource(*source, setup.SharedRegistry, setup.SecretStores)
		if mintErr != nil {
			pf(stderr, "error: configure workflow-source GitHub App authentication: %v\n", mintErr)
			return 1
		}
		workflowSourceAppTokens = minted
	}
	var sourceReconcileMu sync.Mutex
	var sourceRevision string
	reconcileApply := func(applyCtx context.Context, now time.Time) applyResponse {
		sourceReconcileMu.Lock()
		defer sourceReconcileMu.Unlock()
		var resp applyResponse
		if source := setup.Config.WorkflowSource; source != nil && source.Kind == instance.WorkflowSourceKindGit {
			revision, _, syncErr := instance.SyncGitWorkflowSource(applyCtx, root, *source, workflowSourceAppTokens, setup.SharedRegistry, setup.SecretStores)
			if syncErr != nil {
				resp.Error = fmt.Sprintf("sync workflow source: %v", syncErr)
				return resp
			}
			resp.Revision = revision
		}
		applied, oldDigest, newDigest, rejected, reloadErr := reloader.pollOnce(now)
		resp.Applied = applied
		resp.OldDigest = oldDigest
		resp.NewDigest = newDigest
		resp.Rejected = rejected
		if reloadErr != nil {
			resp.Error = reloadErr.Error()
		} else if resp.Revision != "" {
			sourceRevision = resp.Revision
		}
		return resp
	}
	applySweep := func() error {
		return sweepPendingApplyRequests(ctx, l.SchedulerDir(), reconcileApply, time.Now)
	}
	applySweepErrors.report(applySweep())

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
	claimTickerDone := make(chan struct{})
	claimSweepErrors := newSweepErrorReporter(setup.InstanceLog, "claim_recovery_failed")
	claimRenewErrors := newSweepErrorReporter(setup.InstanceLog, "claim_renewal_failed")
	go func() {
		defer close(claimTickerDone)
		defer claimTicker.Stop()
		for {
			select {
			case <-ctx.Done():
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
				probeErr, renewErr := rebuildClaimRenewalSet(ctx, l, claimLiveness, claimRecoveryGate)
				if isJournaledClaimsLockTimeout(renewErr) {
					claimRenewErrors.report(nil)
				} else if renewErr != nil {
					claimRenewErrors.report(renewErr)
				} else {
					claimRenewErrors.report(probeErr)
				}
				released, err := recoverExpiredClaims(now)
				if isJournaledClaimsLockTimeout(err) {
					claimSweepErrors.report(nil)
				} else {
					claimSweepErrors.report(err)
				}
				if err == nil && len(released) > 0 {
					_ = setup.InstanceLog.Append(journal.Event{
						Type:   journal.EventClaimReleased,
						Reason: fmt.Sprintf("periodic recovery released %d expired claim(s)", len(released)),
						Runner: map[string]any{"releasedClaims": len(released)},
					})
				}
			}
		}
	}()

	stalledTicker := time.NewTicker(stalledRunSweepInterval)
	stalledTickerDone := make(chan struct{})
	go func() {
		defer close(stalledTickerDone)
		defer stalledTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-stalledTicker.C:
				stalledSweepErrors.report(sweepStalled(now))
			}
		}
	}()

	telemetryRetentionTicker := time.NewTicker(telemetryRetentionSweepInterval)
	telemetryRetentionTickerDone := make(chan struct{})
	telemetryRetentionErrors := newSweepErrorReporter(setup.InstanceLog, "telemetry_retention_sweep_failed")
	go func() {
		defer close(telemetryRetentionTickerDone)
		defer telemetryRetentionTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-telemetryRetentionTicker.C:
				_, err := pruneConfiguredTelemetryRetention(l, telemetryRetentionConfig, setup.RollupDB, now)
				if err == nil {
					err = compactSchedulerRetention(ctx, telemetryRetentionConfig, setup.RollupDB, setup.InstanceLog, now)
				}
				telemetryRetentionErrors.report(err)
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
	worktreeRetentionTickerDone := make(chan struct{})
	worktreeRetentionErrors := newSweepErrorReporter(setup.InstanceLog, "worktree_retention_sweep_failed")
	go func() {
		defer close(worktreeRetentionTickerDone)
		defer worktreeRetentionTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-worktreeRetentionTicker.C:
				worktreeRetentionErrors.report(sweepWorktreeRetention(ctx, l, setup))
			}
		}
	}()

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
	delegationTickerDone := make(chan struct{})
	go func() {
		defer close(delegationTickerDone)
		defer delegationTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-delegationTicker.C:
				triggerSweepErrors.report(sweepPendingTriggers(ctx, l.SchedulerDir(), sched, time.Now))
				claimAdminSweepErrors.report(sweepPendingClaimAdminRequests(l.SchedulerDir(), setup.InstanceLog, time.Now))
			}
		}
	}()

	// #831's cancel sweep runs on its own ticker so a slow (wedged-stage)
	// cancellation never delays the trigger/claim delegation sweeps above.
	cancelTicker := time.NewTicker(delegationSweepInterval)
	cancelTickerDone := make(chan struct{})
	go func() {
		defer close(cancelTickerDone)
		defer cancelTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-cancelTicker.C:
				cancelSweepErrors.report(cancelSweep())
			}
		}
	}()

	// #459's apply sweep runs on its own ticker for the same reason cancel's
	// does: a slow reconcile (a remote git fetch) must never delay the
	// trigger/claim delegation sweeps it shares no ticker with.
	applyTicker := time.NewTicker(delegationSweepInterval)
	applyTickerDone := make(chan struct{})
	go func() {
		defer close(applyTickerDone)
		defer applyTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-applyTicker.C:
				applySweepErrors.report(applySweep())
			}
		}
	}()
	// #3806: the initial synchronous trigger/claim-admin/cancel/apply sweeps
	// above already ran once, and every one of their periodic tickers is now
	// live.
	sweepsStarted.Store(true)

	supervisorStop := make(chan error, 1)
	supervisorStopDone := make(chan struct{})
	go func() {
		defer close(supervisorStopDone)
		ticker := time.NewTicker(delegationSweepInterval)
		defer ticker.Stop()
		for {
			requested, err := selfupdate.ConsumeStopRequest(root)
			if err != nil || requested {
				supervisorStop <- err
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Plain materialized-directory watching remains opt-in. A Git workflow
	// source reconciles continuously: polling is the availability floor and a
	// local ref watcher provides low-latency wakeups.
	configDone := make(chan error, 1)
	configLoopEnabled := *watchConfig
	if source := setup.Config.WorkflowSource; source != nil && source.Kind == instance.WorkflowSourceKindGit {
		configLoopEnabled = true
		sourceLoop := &configSourceReconciler{
			source: *source,
			errors: newSweepErrorReporter(setup.InstanceLog, "config_reconcile_failed"),
			wake:   sourceReconcileWake,
			reconcile: func(reconcileCtx context.Context, now time.Time) error {
				sourceReconcileMu.Lock()
				defer sourceReconcileMu.Unlock()
				revision, changed, _, syncErr := instance.SyncGitWorkflowSourceIfChanged(
					reconcileCtx,
					root,
					*source,
					sourceRevision,
					workflowSourceAppTokens,
					setup.SharedRegistry,
					setup.SecretStores,
				)
				if syncErr != nil {
					return fmt.Errorf("sync workflow source: %w", syncErr)
				}
				if !changed {
					return nil
				}
				_, _, _, _, reloadErr := reloader.pollOnce(now)
				if reloadErr != nil {
					return reloadErr
				}
				sourceRevision = revision
				return nil
			},
		}
		go func() { configDone <- sourceLoop.Run(ctx) }()
	} else if *watchConfig {
		go func() { configDone <- reloader.Run(ctx) }()
	}

	if err := publishDaemonAPIAddress(apiAddressPath, apiServer.Address()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	apiAddressPublished := true
	defer func() {
		if apiAddressPublished {
			if err := removeDaemonAPIAddress(apiAddressPath); err != nil {
				pf(stderr, "error: %v\n", err)
			}
		}
	}()

	if webhookGate.Start() {
		ready.Store(true)
	}
	pf(stdout, "daemon started at %s (%d workflow(s)); API listening at %s://%s%s\n", root, len(setup.Entries), apiServer.Scheme(), apiServer.Address(), httpapi.Prefix)
	if webhookServer != nil {
		pf(stdout, "GitHub webhooks listening at http://%s%s\n", webhookServer.Address(), webhookhttp.Path)
	}
	if diagnosticsMode {
		pln(stdout, "diagnostics mode: ON — long-running stages get periodic process samples + lsof + un-truncated output recorded as run artifacts")
	}
	var heartbeatDone <-chan struct{}
	if !*quiet {
		tail, tailErr := journal.OpenInstanceLogTail(l.SchedulerDir())
		done := make(chan struct{})
		heartbeatDone = done
		go emitHeartbeats(ctx, stdout, l.SchedulerDir(), len(setup.Entries), tail, tailErr, heartbeatInterval, done)
	}
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- sched.Run(ctx) }()
	var runErr error
	schedulerFailed := false
	apiFailed := false
	webhookFailed := false
	configFailed := false
	configWatcherDone := false
	var webhookErrors <-chan error
	if webhookServer != nil {
		webhookErrors = webhookServer.Errors()
	}
	select {
	case runErr = <-schedulerDone:
	case stopErr := <-supervisorStop:
		if stopErr != nil {
			schedulerFailed = true
			pf(stderr, "error: supervisor stop request: %v\n", stopErr)
		}
		stopDaemon()
		runErr = <-schedulerDone
	case reloadErr := <-configDone:
		configWatcherDone = true
		if reloadErr == nil {
			reloadErr = errors.New("config watcher stopped unexpectedly")
		}
		if ctx.Err() == nil {
			configFailed = true
			pf(stderr, "error: config watcher stopped: %v\n", reloadErr)
		}
		stopDaemon()
		runErr = <-schedulerDone
	case serveErr, ok := <-apiServer.Errors():
		apiFailed = true
		if !ok {
			serveErr = errors.New("server stopped unexpectedly")
		}
		pf(stderr, "error: HTTP API stopped: %v\n", serveErr)
		stopDaemon()
		runErr = <-schedulerDone
	case serveErr, ok := <-webhookErrors:
		webhookFailed = true
		if !ok {
			serveErr = errors.New("server stopped unexpectedly")
		}
		pf(stderr, "error: webhook listener stopped: %v\n", serveErr)
		stopDaemon()
		runErr = <-schedulerDone
	}
	stopDaemon()
	if configLoopEnabled && !configWatcherDone {
		if reloadErr := <-configDone; reloadErr != nil {
			configFailed = true
			pf(stderr, "error: config watcher stopped: %v\n", reloadErr)
		}
	}
	openPRs.Stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	shutdownErr := apiServer.Shutdown(shutdownCtx)
	var webhookShutdownErr error
	if webhookServer != nil {
		webhookShutdownErr = webhookServer.Shutdown(shutdownCtx)
	}
	shutdownCancel()
	apiStopped = true
	if shutdownErr != nil {
		apiFailed = true
		pf(stderr, "error: %v\n", shutdownErr)
	}
	if webhookShutdownErr != nil {
		webhookFailed = true
		pf(stderr, "error: shut down webhook listener: %v\n", webhookShutdownErr)
	}
	if err := removeDaemonAPIAddress(apiAddressPath); err != nil {
		apiFailed = true
		pf(stderr, "error: %v\n", err)
	} else {
		apiAddressPublished = false
	}

	// Wait for both background goroutines to fully stop BEFORE any further
	// stdout/stderr writes below: each reacts to the same ctx cancellation
	// independently, so without this join a tick still in flight when
	// sched.Run returns would race the writes below on the shared io.Writer
	// (stdout/stderr are not safe for concurrent use).
	<-claimTickerDone
	<-stalledTickerDone
	<-telemetryRetentionTickerDone
	<-worktreeRetentionTickerDone
	<-delegationTickerDone
	<-cancelTickerDone
	<-applyTickerDone
	<-supervisorStopDone
	if heartbeatDone != nil {
		<-heartbeatDone
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
		schedulerFailed = true
		pf(stderr, "error: scheduler stopped: %v\n", runErr)
	}

	drainResult := drainDaemonRuns(&wg, sched.Wait, setup.RunnerRegistry, *drainTimeout, force, stdout,
		func(active []trackedRun) []parkedRun { return parkedNonTerminalRuns(l, active) })
	if !drainResult.forced {
		pln(stdout, "shutdown complete: all runs drained")
	} else {
		pf(stdout, "hard shutdown complete: %d run(s) stopped; they will resume from their last checkpoints on the next `goobers up`\n", drainResult.terminated)
	}
	if apiFailed || webhookFailed || configFailed || schedulerFailed {
		return 1
	}
	if !drainResult.forced {
		if err := journalDaemonCleanShutdown(setup.InstanceLog, currentDaemon); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
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
		wg.Wait()
		waitScheduler()
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
	options = append(options, additionalOptions...)
	return localscheduler.New(setup.Entries, setup.InstanceLog, options...)
}

func publishDaemonAPIAddress(path, address string) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+daemonAPIAddressFileName+"-*")
	if err != nil {
		return fmt.Errorf("create daemon API address file: %w", err)
	}
	tempPath := file.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.WriteString(file, address+"\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write daemon API address file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close daemon API address file: %w", err)
	}
	if err := durability.ReplaceFile(tempPath, path); err != nil {
		return fmt.Errorf("publish daemon API address: %w", err)
	}
	removeTemp = false
	return nil
}

func removeDaemonAPIAddress(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon API address: %w", err)
	}
	return nil
}

func worktreeRunTerminal(runsDir string) func(string) (bool, error) {
	return func(worktreeID string) (bool, error) {
		phase, found, err := retainedWorktreePhase(runsDir, worktreeID, "")
		return found && terminalRunPhase(phase), err
	}
}

type heartbeatActivity struct {
	triggers int
	started  int
	finished int
	skipped  int
}

func summarizeHeartbeat(events []journal.Event, afterSeq uint64) (heartbeatActivity, uint64) {
	activity := heartbeatActivity{}
	lastSeq := afterSeq
	for _, event := range events {
		if event.Seq <= afterSeq {
			continue
		}
		if event.Seq > lastSeq {
			lastSeq = event.Seq
		}
		switch event.Type {
		case journal.EventTriggerFired:
			activity.triggers++
		case journal.EventRunStarted:
			activity.started++
		case journal.EventRunFinished:
			activity.finished++
		case journal.EventTickSkipped:
			activity.skipped++
		}
	}
	return activity, lastSeq
}

func emitHeartbeats(
	ctx context.Context,
	stdout io.Writer,
	schedulerDir string,
	workflowCount int,
	tail *journal.InstanceLogTail,
	err error,
	interval time.Duration,
	done chan<- struct{},
) {
	defer close(done)
	defer func() { _ = tail.Close() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if tail == nil {
				tail, err = journal.OpenInstanceLogTail(schedulerDir)
			}
			if err == nil {
				var events []journal.Event
				events, err = tail.Events()
				if err == nil {
					activity, _ := summarizeHeartbeat(events, 0)
					pf(stdout, "[%s] alive — %d workflow(s), %d trigger(s) fired, %d run(s) started, %d run(s) finished, %d tick(s) skipped\n",
						now.Format("15:04:05"), workflowCount, activity.triggers, activity.started, activity.finished, activity.skipped)
					continue
				}
				_ = tail.Close()
				tail = nil
			}
			if err != nil {
				pf(stdout, "[%s] alive — scheduler activity unavailable: %v\n", now.Format("15:04:05"), err)
				continue
			}
		}
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
