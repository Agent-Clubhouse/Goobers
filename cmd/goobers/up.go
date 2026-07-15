package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
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

// reapStaleAfter bounds how long a Keep-on-failure worktree survives before
// Manager.Reap sweeps it up too, on top of genuine crash orphans (issue
// #136) — nothing in the runner sets RemoveOptions.Keep yet, so this only
// matters once something does; a day gives an operator time to look at one
// before it's reclaimed.
const reapStaleAfter = 24 * time.Hour

func runUp(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runUpContext(ctx, args, stdout, stderr)
}

// runUpContext is runUp's testable core: the OS signal wiring lives only in
// runUp, so tests can drive shutdown deterministically via ctx cancellation
// instead of sending real signals.
func runUpContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers up [path]\n\n"+
			"Run the daemon: the embedded scheduler (cron triggers + run conditions)\n"+
			"plus the local runner loop (default path \".\"). Blocks until interrupted\n"+
			"(SIGINT/SIGTERM), then drains in-flight runs before exiting. Exit codes:\n"+
			"0 = clean shutdown, 1 = daemon startup failed, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
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
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer release()

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer setup.Shutdown(context.Background())

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

	// Reap crash-orphaned worktrees before anything tries to resume into one
	// of their keys (issue #136): a mid-stage crash otherwise leaves a
	// worktree directory that makes worktree.Create refuse forever (fixed
	// separately by adopt-and-reset, but Reap is still what actually reclaims
	// the disk space and the git worktree-list registration).
	if _, warnings, err := setup.Worktrees.Reap(ctx, worktree.ReapOptions{StaleAfter: reapStaleAfter}); err != nil {
		pf(stderr, "error: reap worktrees: %v\n", err)
		return 1
	} else {
		for _, w := range warnings {
			pf(stdout, "warning: unreadable worktree marker %s: %v\n", w.Path, w.Err)
		}
	}

	// Reconcile BEFORE the resume scan (issue #135): it seeds Conditions'
	// active-run counts from the very same non-terminal runs the resume scan
	// is about to act on, so each resumed run's Release call (below) has a
	// reserved slot to actually release — reversing this order would let the
	// resume scan's Releases race Reconcile's blind Conditions.Reconcile
	// overwrite and land before the slot even exists.
	opts := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(setup.RunConditions.MaxParallelRuns, setup.RunConditions.WorkflowBudgets, setup.RunConditions.WorkflowDailyBudgets))
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, opts...)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	// Crash-resume: any run left non-terminal by a prior crash or unclean
	// shutdown restarts now, before the scheduler starts admitting new ticks
	// (#23 AC: restart via Runner.Resume). A run whose workflow no longer
	// resolves in config is skipped with a warning (issue #135), not fatal —
	// recover it with `goobers run abort <run-id>`. Each resumed run also
	// incrementally ingests into the telemetry rollup once its outcome is
	// known (issue #127).
	resumed, warned, err := resumeInterruptedRuns(ctx, l, setup.Runner, setup.Machines, setup.RepoRefs, setup.InstanceLog, setup.Telemetry, setup.RollupDB, sched.Release, &wg)
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

	// The periodic sweep runs on its own goroutine for the daemon's entire
	// lifetime, concurrently with the main goroutine's own stdout/stderr
	// writes (both "daemon started" above and the shutdown messages below) —
	// io.Writer implementations like *bytes.Buffer (tests) are not safe for
	// concurrent use, so this goroutine deliberately never writes to
	// stdout/stderr itself (unlike the startup sweep above, which runs
	// synchronously before this goroutine exists and so writes safely).
	// Recovery failures are swallowed here the same way ClaimLedger's own
	// journal() best-effort observability is (claim.go's doc) — routine
	// background maintenance, not a run-affecting operation.
	claimTicker := time.NewTicker(claimRecoverInterval)
	claimTickerDone := make(chan struct{})
	go func() {
		defer close(claimTickerDone)
		defer claimTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-claimTicker.C:
				_, _ = recoverExpiredClaims(now)
			}
		}
	}()

	pf(stdout, "daemon started at %s (%d workflow(s))\n", root, len(setup.Entries))
	runErr := sched.Run(ctx) // blocks until ctx is cancelled

	// Wait for the claim-recovery goroutine to fully stop BEFORE any further
	// stdout/stderr writes below: both it and this goroutine react to the
	// same ctx cancellation independently, so without this join a tick still
	// in flight when sched.Run returns would race the writes below on the
	// shared io.Writer (stdout/stderr are not safe for concurrent use).
	<-claimTickerDone

	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
		pf(stderr, "error: scheduler stopped: %v\n", runErr)
	}

	pln(stdout, "shutting down: draining in-flight runs...")
	if waitDrained(&wg, drainGrace) {
		pln(stdout, "shutdown complete: all runs drained")
	} else {
		pf(stdout, "shutdown timed out after %s: some runs may still be checkpointing\n", drainGrace)
	}
	return 0
}
