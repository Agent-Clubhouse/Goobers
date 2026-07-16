package main

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/signals"
)

// runPollInterval bounds how often waitForRunTerminal re-reads a run's
// journal while `goobers run` blocks on it. Var, not const, so tests don't
// have to wait out a real 200ms per poll.
var runPollInterval = 200 * time.Millisecond

func exitForPhase(phase journal.RunPhase) int {
	switch phase {
	case journal.PhaseCompleted:
		return 0
	case journal.PhaseFailed, journal.PhaseAborted:
		return 1
	case journal.PhaseEscalated:
		return 3
	default:
		return 1
	}
}

func runRun(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "abort" {
		return runRunAbort(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noWait := fs.Bool("no-wait", false, "return after the run is dispatched")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers run <workflow> [--no-wait] [path]\n"+
			"       goobers run abort <run-id> [path]\n\n"+
			"Trigger a run of a config/ workflow manually, through the same scheduler\n"+
			"(run conditions, instance journal, single-instance lock) a live `goobers up`\n"+
			"daemon uses, then wait for it to reach a terminal state unless\n"+
			"--no-wait is set (default path \".\"). If a live `goobers up` daemon already\n"+
			"holds the instance lock,\n"+
			"delegates the trigger to it instead of failing (#343) — dispatched through\n"+
			"the same Scheduler.Trigger path either way. Exit codes after waiting: 0 =\n"+
			"completed, 1 = failed/aborted or business error (unknown workflow, invalid\n"+
			"config, run conditions rejected the trigger), 2 = usage/IO error, 3 =\n"+
			"escalated. A successful submission-only mode (such as --no-wait, once\n"+
			"available) exits 0 because it does not observe a terminal phase.\n"+
			"`run abort` marks a stuck non-terminal run aborted directly in its own\n"+
			"journal — recovery for a run resumeInterruptedRuns can't resolve on its own.\n")
	}
	if err := fs.Parse(runFlagArgs(args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}

	ctx, stop := signals.SetupSignalContext()
	defer stop()

	// Take the same single-instance lock `up` does (issue #134): a manual run
	// must not mutate scheduler/run-condition/claim-ledger state, or the
	// shared workcopies/ tree, concurrently with a live daemon. When a live
	// daemon already holds the lock, delegate through the file-based
	// protocol in rundelegate.go instead of failing (#343 — #231 only fixed
	// the error text; this is the actual behavior fix) rather than requiring
	// the daemon stopped first.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		return runDelegatedTrigger(ctx, l, name, root, *noWait, stdout, stderr)
	}
	if *noWait && runProcessExits {
		release()
		return runDetachedTrigger(ctx, l, name, root, stdout, stderr)
	}
	return runStandaloneTrigger(ctx, l, name, root, *noWait, false, release, stdout, stderr)
}

// runStandaloneTrigger owns the one-shot scheduler and instance lock. A real
// detached worker stays alive until Starter.Start returns so paused runs
// release those resources; in-process callers hand that cleanup to a goroutine.
func runStandaloneTrigger(ctx context.Context, l instance.Layout, name, root string, noWait, worker bool, release func(), stdout, stderr io.Writer) int {
	releaseOnReturn := true
	defer func() {
		if releaseOnReturn {
			release()
		}
	}()

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	shutdownOnReturn := true
	defer func() {
		if shutdownOnReturn {
			setup.Shutdown(context.Background())
		}
	}()

	found := false
	var gaggle string
	for _, e := range setup.Entries {
		if e.Workflow == name {
			found = true
			gaggle = e.Gaggle
			break
		}
	}
	if !found {
		pf(stderr, "error: no workflow named %q in %s\n", name, l.ConfigDir())
		return 1
	}

	opts := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(setup.RunConditions.MaxParallelRuns, setup.RunConditions.WorkflowBudgets, setup.RunConditions.WorkflowDailyBudgets))
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, opts...)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	triggerCtx := ctx
	if noWait && !worker {
		triggerCtx = context.WithoutCancel(ctx)
	}
	runID, err := sched.Trigger(triggerCtx, name, time.Now())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "created run %s (workflow=%s gaggle=%s)\n", runID, name, gaggle)
	if noWait {
		shutdownOnReturn = false
		releaseOnReturn = false
		cleanup := func() {
			sched.Wait()
			setup.Shutdown(context.Background())
			release()
		}
		pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
		if worker {
			cleanup()
		} else {
			go cleanup()
		}
		return 0
	}

	phase, err := waitForRunTerminal(ctx, l.RunsDir(), runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	// waitForRunTerminal polls the run's OWN journal and returns as soon as
	// it sees a terminal phase — that races trackedStarter.Start's dispatch
	// goroutine, which still has its post-completion telemetry ingest
	// (ingestRunTelemetry) to run before it calls wg.Done(). Waiting for wg
	// here (this run is the only dispatch `goobers run` ever tracks) closes
	// that gap, so `goobers trace` run immediately afterward reliably sees
	// this run's rollup rows without needing a separate --rebuild.
	wg.Wait()

	pf(stdout, "finished: phase=%s\n", phase)
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
	return exitForPhase(phase)
}

// runDelegatedTrigger is #343's actual fix: called when acquireInstanceLock
// finds a live `goobers up` daemon already holding this instance's lock — it
// no longer just reports that and gives up (#231's fix stopped there). It
// writes a delegation request (rundelegate.go) the daemon's own periodic
// sweep picks up and dispatches through the identical Scheduler.Trigger path
// this process would have called itself, then waits for a response and the
// dispatched run's terminal state unless noWait is set. From the caller's
// perspective the two paths are otherwise indistinguishable except for which
// process actually held the scheduler.
func runDelegatedTrigger(ctx context.Context, l instance.Layout, name, root string, noWait bool, stdout, stderr io.Writer) int {
	requestID, err := writeTriggerRequest(l.SchedulerDir(), name)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	runID, err := pollTriggerResponse(ctx, l.SchedulerDir(), requestID, triggerDelegationTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "created run %s (workflow=%s, dispatched via live daemon)\n", runID, name)
	if noWait {
		pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
		return 0
	}

	phase, err := waitForRunTerminal(ctx, l.RunsDir(), runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stdout, "finished: phase=%s\n", phase)
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
	return exitForPhase(phase)
}

// runFlagArgs lets --no-wait appear after the workflow, as documented. The
// standard flag package otherwise stops parsing at the first positional arg.
func runFlagArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--no-wait" || arg == "-no-wait" ||
			strings.HasPrefix(arg, "--no-wait=") || strings.HasPrefix(arg, "-no-wait=") {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

// runRunAbort marks a stuck non-terminal run as aborted by appending a
// terminal run.finished(status=aborted) event directly to its own journal —
// issue #135's sanctioned recovery path for a run resumeInterruptedRuns
// can't resolve on its own (e.g. its workflow was renamed/removed from
// config, so `goobers up` skips it with a warning forever rather than
// erroring at startup). Works on the run's journal alone — it doesn't need
// the run's workflow to still exist in config, unlike everything else in
// this file.
func runRunAbort(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run abort", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers run abort <run-id> [path]\n\n"+
			"Mark a stuck non-terminal run aborted by appending a terminal\n"+
			"run.finished(status=aborted) event to its own journal (default path\n"+
			"\".\"). Exit codes: 0 = aborted, 1 = business error (run already terminal),\n"+
			"2 = usage/IO error (unknown run).\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	runID := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	// runID is raw CLI input, joined onto RunsDir below and then used to
	// append a terminal event — a traversal id (e.g. "../../x") must not
	// touch anything outside the instance (#244).
	if !apiv1.ValidRunID(runID) {
		pf(stderr, "error: invalid run id %q\n", runID)
		return 2
	}

	l := instance.NewLayout(root)
	dir := filepath.Join(l.RunsDir(), runID)

	if reader, err := journal.OpenRead(dir); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	} else if phase, err := reader.Phase(); err == nil {
		// Event-log-first (#242): a stale state.json can still claim
		// {running, ...} after a crash-fsynced run.finished — trusting it
		// here would let abort append a SECOND run.finished onto an
		// already-terminal run, flipping its recorded terminal phase.
		switch phase {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			if err := releaseClaimsForRun(l, nil, runID); err != nil {
				pf(stderr, "error: release claims for terminal run %s: %v\n", runID, err)
				return 2
			}
			pf(stderr, "error: run %s is already terminal (phase=%s)\n", runID, phase)
			return 1
		}
	}

	registrar, scrubber := journal.DefaultScrubber()
	run, _, err := journal.Recover(dir, journal.WithScrubber(scrubber))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = run.Close() }()
	if err := prepareAbortedRunBranch(l, runID, run, registrar); err != nil {
		pf(stderr, "warning: terminal branch cleanup for run %s: %v\n", runID, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if err := releaseClaimsForRun(l, nil, runID); err != nil {
		pf(stderr, "error: release claims for aborted run %s: %v\n", runID, err)
		return 2
	}

	pf(stdout, "aborted run %s\n", runID)
	return 0
}

// waitForRunTerminal polls runID's journal until it reaches a terminal phase
// or ctx is cancelled. Scheduler.Trigger's own dispatch continues
// asynchronously in a background goroutine (issue #134) — this is what
// preserves `goobers run`'s existing block-until-done UX rather than
// returning the instant the run is merely admitted. A run that pauses at a
// human gate (or a daemon-drain checkpoint, though none applies here since
// `run` holds its own instance lock) stays PhaseRunning indefinitely by
// design; ctx cancellation (SIGINT/SIGTERM) is what lets a caller stop
// waiting on it, reporting its phase as of that moment.
func waitForRunTerminal(ctx context.Context, runsDir, runID string) (journal.RunPhase, error) {
	dir := filepath.Join(runsDir, runID)
	for {
		if reader, err := journal.OpenRead(dir); err == nil {
			switch phase := runPhase(reader); phase {
			case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
				return phase, nil
			}
		}
		select {
		case <-ctx.Done():
			if reader, err := journal.OpenRead(dir); err == nil {
				return runPhase(reader), nil
			}
			return journal.PhaseRunning, ctx.Err()
		case <-time.After(runPollInterval):
		}
	}
}
