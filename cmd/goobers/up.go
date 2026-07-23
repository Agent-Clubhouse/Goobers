package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/signals"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/winsvc"
	"github.com/goobers/goobers/internal/worktree"
)

// drainGrace bounds how long runUpContext waits, after its context is
// cancelled, for in-flight Start/Resume goroutines to checkpoint and return
// before exiting anyway (issue #23 AC: graceful drain, not an indefinite
// hang if a stage is wedged). Var, not const, so tests can shrink it rather
// than waiting out a real 30s.
var drainGrace = 30 * time.Second

// claimRecoverInterval bounds how often runUpContext sweeps the claim ledger
// for expired leases while running, catching a live run that overran its
// lease without crashing (localscheduler.ClaimLedger.RecoverExpired's doc:
// "call once at startup... and periodically thereafter"). Var, not const, so
// tests can shrink it rather than waiting out a real 5 minutes.
var claimRecoverInterval = 5 * time.Minute

// stalledRunSweepInterval bounds how quickly the daemon notices a run that has
// crossed its configured journal-silence deadline.
var stalledRunSweepInterval = time.Minute

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
	message := err.Error()
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
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runUpContext(ctx, args, stdout, stderr)
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

const upHelp = "Usage: goobers up [--quiet] [--diagnostics] [--notify[=all]] [--skip-preflight] [--cleanup-spans-only-runs] [path]\n\n" +
	"Run the daemon: the embedded scheduler (cron triggers + run conditions)\n" +
	"plus the local runner, loopback HTTP API, and configured GitHub webhook\n" +
	"listener (default path \".\"). Blocks\n" +
	"until interrupted (SIGINT/SIGTERM), then drains in-flight runs before\n" +
	"exiting. Exit codes: 0 = clean shutdown, 1 = daemon/API failure,\n" +
	"2 = usage/IO error.\n\n" +
	"Legacy spans-only run directories are reported as cleanup candidates\n" +
	"and preserved by default. --cleanup-spans-only-runs deletes them at\n" +
	"startup after reporting each candidate.\n\n" +
	"Startup validates the resolved instance config and refuses to run on\n" +
	"errors. --skip-preflight bypasses that refusal with a prominent warning.\n\n" +
	"--diagnostics turns on deep, opt-in capture for hard hangs: any\n" +
	"deterministic stage still running past a couple of minutes gets a\n" +
	"periodic native process sample + process tree + open-fd (lsof)\n" +
	"snapshot recorded as a run artifact, and stage stdout/stderr are kept\n" +
	"un-truncated. Verbose and slightly heavier; leave off for normal runs.\n"

// runUpContext is runUp's testable core: the OS signal wiring lives only in
// runUp, so tests can drive shutdown deterministically via ctx cancellation
// instead of sending real signals.
func runUpContext(parentCtx context.Context, args []string, stdout, stderr io.Writer) int {
	webhookGate, err := webhookhttp.NewDispatchGate(parentCtx)
	if err != nil {
		pf(stderr, "error: initialize daemon lifecycle: %v\n", err)
		return 1
	}
	ctx := webhookGate.Context()
	var ready atomic.Bool
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

	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "up")
	quiet := fs.Bool("quiet", false, "suppress periodic liveness heartbeats")
	diagnostics := fs.Bool("diagnostics", false, "capture deep per-stage diagnostics (process samples, lsof, un-truncated output) for hang debugging")
	watchConfig := fs.Bool("watch-config", false, "experimental: hot-reload config edits without a restart (default off; superseded by the Workflow CD config source, #453)")
	var notifications notifyFlag
	fs.Var(&notifications, "notify", "send desktop notifications for escalated and failed runs; use --notify=all for every terminal outcome")
	skipPreflight := fs.Bool("skip-preflight", false, "start despite instance config validation errors (unsafe)")
	cleanupSpansOnlyRuns := fs.Bool("cleanup-spans-only-runs", false, "delete reported legacy spans-only run directories at startup")
	if err := fs.Parse(args); err != nil {
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

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	if code := runStartupConfigPreflight(root, *skipPreflight, stderr); code != 0 {
		return code
	}

	// Single-instance lock (#23 AC3): a second `up` on the same instance root
	// must fail fast with a clear message, not silently race the first.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	release, err := acquireDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"), root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer release()
	apiAddressPath := filepath.Join(l.SchedulerDir(), daemonAPIAddressFileName)
	if err := removeDaemonAPIAddress(apiAddressPath); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	var wg sync.WaitGroup
	var setup *schedulerSetup
	if *skipPreflight {
		setup, err = buildSchedulerSetupAllowingInvalidConfig(ctx, l, &wg, withDesktopNotifications(notifications, stderr))
	} else {
		setup, err = buildSchedulerSetup(ctx, l, &wg, withDesktopNotifications(notifications, stderr))
	}
	if err != nil {
		printValidationIssues(stderr, validationReportFromError(err))
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer setup.Shutdown(context.Background())
	if err := journalValidationWarnings(setup.InstanceLog, setup.Validation.Warnings()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
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
	}, ready.Load)
	if err != nil {
		pf(stderr, "error: initialize read service: %v\n", err)
		return 1
	}
	apiLog := log.New(stderr, "http API: ", log.LstdFlags)
	eventStream, err := httpapi.NewEventStream(l, apiLog)
	if err != nil {
		pf(stderr, "error: initialize HTTP event stream: %v\n", err)
		return 1
	}
	defer eventStream.Close()
	handler, err := httpapi.NewHandler(reads, httpapi.AllowAll, apiLog, httpapi.WithEventStream(eventStream))
	if err != nil {
		pf(stderr, "error: initialize HTTP API: %v\n", err)
		return 1
	}
	apiServer, err := httpapi.NewServer(apiListenAddress(setup.Config), handler, apiLog)
	if err != nil {
		pf(stderr, "error: initialize HTTP API: %v\n", err)
		return 1
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
		return recoverClaims(l, setup.InstanceLog, now)
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

	// Reconcile BEFORE the resume scan (issue #135): it seeds Conditions'
	// active-run counts from the very same non-terminal runs the resume scan
	// is about to act on, so each resumed run's ReleaseReconciled call (below)
	// has a reserved slot to actually release.
	opts := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(setup.RunConditions.MaxParallelRuns, setup.RunConditions.WorkflowBudgets, setup.RunConditions.WorkflowDailyBudgets))
	// #353: start the open-PR-count refresher and wire it as the MaxOpenPRs cap's
	// counter. Runs on its own interval/context under the daemon's WaitGroup, so
	// Admit reads a cached count (never a network call under the tick lock) and
	// shutdown drains it with every other background loop. Nil when no workflow
	// opts into the cap.
	if setup.OpenPRRefresher != nil {
		opts = append(opts, localscheduler.WithOpenPRCounter(setup.OpenPRRefresher))
	}

	sched := localscheduler.New(setup.Entries, setup.InstanceLog, opts...)
	webhookLog := log.New(stderr, "webhook: ", log.LstdFlags)
	webhookServer, err := buildWebhookServer(ctx, setup, sched, webhookGate, webhookLog)
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
	stalledRunTimeout, err := setup.RunConditions.StalledRunTimeoutDuration()
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
				return buildTerminalBranchPreparer(runLayout, setup.Config, setup.SharedRegistry)
			},
			setup.TerminalNotifier,
			sched.ReleaseRun,
			now,
			stalledRunTimeout,
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

	// Crash-resume: any run left non-terminal by a prior crash or unclean
	// shutdown restarts now, before the scheduler starts admitting new ticks
	// (#23 AC: restart via Runner.Resume). A run whose workflow no longer
	// resolves in config is skipped with a warning (issue #135), not fatal —
	// recover it with `goobers run abort <run-id>`. Each resumed run also
	// incrementally ingests into the telemetry rollup once its outcome is
	// known (issue #127).
	resumed, warned, err := resumeInterruptedRunsWithRunners(ctx, l, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	for _, runID := range resumed {
		pf(stdout, "resuming interrupted run %s\n", runID)
	}
	for _, runID := range warned {
		pf(stdout, "warning: run %s references a workflow no longer in config — skipped; recover with `goobers run abort %s`\n", runID, runID)
	}

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
	go func() {
		defer close(claimTickerDone)
		defer claimTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-claimTicker.C:
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

	// Config hot-reload is opt-in. Off by default, `goobers up` keeps the V0
	// load-once-at-startup behavior; with --watch-config the daemon watches the
	// config dir and swaps validated edits live. This gate is interim: the
	// Workflow CD config source (#453) replaces both the flag and this poll loop
	// with an instance-level workflowSource once that epic lands.
	configDone := make(chan error, 1)
	if *watchConfig {
		reloader := &configReloader{
			layout:         l,
			setup:          setup,
			scheduler:      sched,
			openPRs:        openPRs,
			reads:          reads,
			events:         eventStream,
			wg:             &wg,
			appliedDigest:  setup.ConfigDigest,
			observedDigest: setup.ConfigDigest,
		}
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
	pf(stdout, "daemon started at %s (%d workflow(s)); API listening at http://%s%s\n", root, len(setup.Entries), apiServer.Address(), httpapi.Prefix)
	if webhookServer != nil {
		pf(stdout, "GitHub webhooks listening at http://%s%s\n", webhookServer.Address(), webhookhttp.Path)
	}
	if diagnosticsMode {
		pln(stdout, "diagnostics mode: ON — long-running stages get periodic process samples + lsof + un-truncated output recorded as run artifacts")
	}
	var heartbeatDone <-chan struct{}
	if !*quiet {
		lastSeq := uint64(0)
		if events, err := journal.ReadInstanceLog(l.SchedulerDir()); err == nil && len(events) > 0 {
			lastSeq = events[len(events)-1].Seq
		}
		done := make(chan struct{})
		heartbeatDone = done
		go emitHeartbeats(ctx, stdout, l.SchedulerDir(), len(setup.Entries), lastSeq, heartbeatInterval, done)
	}
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- sched.Run(ctx) }()
	var runErr error
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
	if *watchConfig && !configWatcherDone {
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
	<-delegationTickerDone
	<-cancelTickerDone
	if heartbeatDone != nil {
		<-heartbeatDone
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
		pf(stderr, "error: scheduler stopped: %v\n", runErr)
	}

	pln(stdout, "shutting down: draining in-flight runs...")
	runsDrained := waitDrained(&wg, drainGrace)
	schedulerDrained := runsDrained && waitSchedulerDrained(sched, drainGrace)
	if runsDrained && schedulerDrained {
		pln(stdout, "shutdown complete: all runs drained")
	} else {
		pf(stdout, "shutdown timed out after %s: some runs may still be checkpointing\n", drainGrace)
	}
	if apiFailed || webhookFailed || configFailed {
		return 1
	}
	return 0
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
	lastSeq uint64,
	interval time.Duration,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			events, err := journal.ReadInstanceLog(schedulerDir)
			if err != nil {
				pf(stdout, "[%s] alive — scheduler activity unavailable: %v\n", now.Format("15:04:05"), err)
				continue
			}
			activity, nextSeq := summarizeHeartbeat(events, lastSeq)
			lastSeq = nextSeq
			pf(stdout, "[%s] alive — %d workflow(s), %d trigger(s) fired, %d run(s) started, %d run(s) finished, %d tick(s) skipped\n",
				now.Format("15:04:05"), workflowCount, activity.triggers, activity.started, activity.finished, activity.skipped)
		}
	}
}
