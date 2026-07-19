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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/signals"
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

// reapStaleAfter bounds how long a Keep-on-failure worktree survives before
// Manager.Reap sweeps it up too, on top of genuine crash orphans (issue
// #136) — nothing in the runner sets RemoveOptions.Keep yet, so this only
// matters once something does; a day gives an operator time to look at one
// before it's reclaimed.
const reapStaleAfter = 24 * time.Hour

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
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runUpContext(ctx, args, stdout, stderr)
}

// runUpContext is runUp's testable core: the OS signal wiring lives only in
// runUp, so tests can drive shutdown deterministically via ctx cancellation
// instead of sending real signals.
func runUpContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers up [--quiet] [--diagnostics] [path]\n\n"+
			"Run the daemon: the embedded scheduler (cron triggers + run conditions)\n"+
			"plus the local runner and loopback HTTP API (default path \".\"). Blocks\n"+
			"until interrupted (SIGINT/SIGTERM), then drains in-flight runs before\n"+
			"exiting. Exit codes: 0 = clean shutdown, 1 = daemon/API failure,\n"+
			"2 = usage/IO error.\n\n"+
			"--diagnostics turns on deep, opt-in capture for hard hangs: any\n"+
			"deterministic stage still running past a couple of minutes gets a\n"+
			"periodic native process sample + process tree + open-fd (lsof)\n"+
			"snapshot recorded as a run artifact, and stage stdout/stderr are kept\n"+
			"un-truncated. Verbose and slightly heavier; leave off for normal runs.\n")
	}
	quiet := fs.Bool("quiet", false, "suppress periodic liveness heartbeats")
	diagnostics := fs.Bool("diagnostics", false, "capture deep per-stage diagnostics (process samples, lsof, un-truncated output) for hang debugging")
	watchConfig := fs.Bool("watch-config", false, "experimental: hot-reload config edits without a restart (default off; superseded by the Workflow CD config source, #453)")
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

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		printValidationIssues(stderr, validationReportFromError(err))
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer setup.Shutdown(context.Background())
	printValidationWarnings(stdout, setup.Validation.CLIWarnings())

	var ready atomic.Bool
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
	// Claim-lease recovery (#131): released once now (recovers leases
	// orphaned by a prior crash) and periodically thereafter (catches a live
	// run that overran its lease without crashing) — before the scheduler
	// starts admitting new ticks, same ordering rationale as crash-resume
	// below. withClaimLock serializes this against a concurrent
	// `goobers backlog-query` subprocess claiming/releasing on the same
	// ledger file (providercmd.go's doc). recoverExpiredClaims itself never
	// touches stdout/stderr — it returns the released entries so ONLY the
	// synchronous startup call site below prints; the periodic goroutine
	// below deliberately does not (see its own comment).
	claimLedgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	claimLockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	recoverExpiredClaims := func(now time.Time) ([]localscheduler.ClaimEntry, error) {
		var released []localscheduler.ClaimEntry
		err := withClaimLock(claimLockPath, func() error {
			ledger, err := localscheduler.OpenClaimLedger(claimLedgerPath, localscheduler.WithInstanceLog(setup.InstanceLog))
			if err != nil {
				return err
			}
			released, err = ledger.RecoverExpired(now)
			return err
		})
		return released, err
	}
	startupReleased, err := recoverExpiredClaims(time.Now())
	if err != nil {
		pf(stderr, "error: recover expired claims: %v\n", err)
		return 1
	}
	for _, entry := range startupReleased {
		pf(stdout, "recovered expired claim %s (was held by run %s)\n", entry.ItemID, entry.RunID)
	}

	// Scratch workspaces have no git metadata to recover. Once this daemon
	// holds the instance lock, every stage-* entry belongs to the prior process
	// and can be removed before interrupted runs allocate fresh workspaces.
	if err := runner.ReapScratchWorkspaces(filepath.Join(l.WorkcopiesDir(), "scratch")); err != nil {
		pf(stderr, "error: reap scratch workspaces: %v\n", err)
		return 1
	}

	// Reap crash-orphaned worktrees before anything tries to resume into one
	// of their keys (issue #136): a mid-stage crash otherwise leaves a
	// worktree directory that makes worktree.Create refuse forever (fixed
	// separately by adopt-and-reset, but Reap is still what actually reclaims
	// the disk space and the git worktree-list registration).
	if _, warnings, err := setup.Worktrees.Reap(ctx, worktree.ReapOptions{
		StaleAfter:    reapStaleAfter,
		IsRunTerminal: worktreeRunTerminal(l.RunsDir()),
	}); err != nil {
		pf(stderr, "error: reap worktrees: %v\n", err)
		return 1
	} else {
		for _, w := range warnings {
			pf(stdout, "warning: skipped worktree cleanup %s: %v\n", w.Path, w.Err)
		}
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
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if err := apiServer.Start(); err != nil {
		pf(stderr, "error: start HTTP API: %v\n", err)
		return 1
	}
	apiStopped := false
	defer func() {
		if apiStopped {
			return
		}
		ready.Store(false)
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
		defer shutdownCancel()
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			pf(stderr, "error: %v\n", err)
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
	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.ReleaseReconciled, &wg)
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
				claimSweepErrors.report(err)
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

	ready.Store(true)
	pf(stdout, "daemon started at %s (%d workflow(s)); API listening at http://%s%s\n", root, len(setup.Entries), apiServer.Address(), httpapi.Prefix)
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
	configFailed := false
	configWatcherDone := false
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
		cancel()
		runErr = <-schedulerDone
	case serveErr, ok := <-apiServer.Errors():
		apiFailed = true
		if !ok {
			serveErr = errors.New("server stopped unexpectedly")
		}
		pf(stderr, "error: HTTP API stopped: %v\n", serveErr)
		cancel()
		runErr = <-schedulerDone
	}
	ready.Store(false)
	cancel()
	if *watchConfig && !configWatcherDone {
		if reloadErr := <-configDone; reloadErr != nil {
			configFailed = true
			pf(stderr, "error: config watcher stopped: %v\n", reloadErr)
		}
	}
	openPRs.Stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	shutdownErr := apiServer.Shutdown(shutdownCtx)
	shutdownCancel()
	apiStopped = true
	if shutdownErr != nil {
		apiFailed = true
		pf(stderr, "error: %v\n", shutdownErr)
	}

	// Wait for both background goroutines to fully stop BEFORE any further
	// stdout/stderr writes below: each reacts to the same ctx cancellation
	// independently, so without this join a tick still in flight when
	// sched.Run returns would race the writes below on the shared io.Writer
	// (stdout/stderr are not safe for concurrent use).
	<-claimTickerDone
	<-delegationTickerDone
	if heartbeatDone != nil {
		<-heartbeatDone
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
		pf(stderr, "error: scheduler stopped: %v\n", runErr)
	}

	pln(stdout, "shutting down: draining in-flight runs...")
	if waitDrained(&wg, drainGrace) {
		pln(stdout, "shutdown complete: all runs drained")
	} else {
		pf(stdout, "shutdown timed out after %s: some runs may still be checkpointing\n", drainGrace)
	}
	if apiFailed || configFailed {
		return 1
	}
	return 0
}

func worktreeRunTerminal(runsDir string) func(string) (bool, error) {
	return func(worktreeID string) (bool, error) {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("read runs directory: %w", err)
		}

		var owner string
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runID := entry.Name()
			if worktreeID != runID && !strings.HasPrefix(worktreeID, runID+"-") {
				continue
			}
			if len(runID) > len(owner) {
				owner = runID
			}
		}
		if owner == "" {
			return false, nil
		}

		rd, err := journal.OpenRead(filepath.Join(runsDir, owner))
		if err != nil {
			return false, err
		}
		phase, err := rd.Phase()
		if err != nil {
			return false, err
		}
		switch phase {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			return true, nil
		default:
			return false, nil
		}
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
