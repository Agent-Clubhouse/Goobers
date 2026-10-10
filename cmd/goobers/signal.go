package main

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/signals"
)

const signalHelp = "Usage: goobers signal [--request-id <key>] <name> [path]\n\n" +
	"Durably accept a named signal and its matching type=signal workflows\n" +
	"through the instance scheduler (default path \".\"). Reuse --request-id\n" +
	"to recover the original recipient set after a lost reply; without it, a\n" +
	"new key is printed before acceptance. A signal may match zero, one, or\n" +
	"many workflows. Waits for each dispatched run to finish or pause.\n" +
	"Capacity-held starts remain queued for goobers up or a same-key retry.\n" +
	"A submission-only receipt still requires dispatch and completion.\n" +
	"The command requires the instance lock; stop its daemon first.\n\n" +
	"Exit codes: 0 = all admitted runs completed or no workflows matched,\n" +
	"1 = a run failed/aborted, a start remains queued, or a business error,\n" +
	"2 = usage/IO error, 3 = a run escalated. Escalation takes precedence\n" +
	"when completed runs have mixed outcomes.\n"

// runSignal accepts a named signal into the host queue and waits for this
// invocation's dispatched recipients. Capacity-held receipts remain durable
// for a later daemon drain or retry with the same request key.
func runSignal(args []string, stdout, stderr io.Writer) (result int) {
	fs := newCLIFlagSet("signal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	requestID := fs.String("request-id", "", "idempotency key for the accepted signal recipient set")
	fs.Usage = helpUsage(stderr, "signal")
	if err := fs.Parse(args); err != nil {
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
	if err := prepareManualRoot(l, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	// Named-signal CLI delivery owns the instance lock. Live daemon delivery
	// continues to use the authenticated webhook listener.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v (a running `goobers up` daemon holds this instance's lock — "+
			"stop it first or use its configured webhook listener)\n", err)
		return 1
	}
	defer release()

	ctx, stop := signals.SetupSignalContext()
	defer stop()

	var wg sync.WaitGroup
	// DS6 for the one-shot path (#3512 review, finding 2): this command holds
	// the instance lock, so the daemon — and with it every claim renewal — is
	// stopped. On an engine-configured instance the setup-time reap plus
	// Claim's expired-lease takeover would both fire on a live distributed
	// run's stale-looking lease, so renewal must run before any
	// scheduling/claiming does. Mode-1 gets a nil recovery: byte-identical
	// recover-at-setup behavior.
	claimRecovery := newOneShotClaimRecovery(l)
	setup, err := buildSchedulerSetup(ctx, l, &wg, claimRecovery.setupOptions()...)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer func() {
		// #3851: surface a lost final flush or close rather than exiting as if
		// the signal command shut down cleanly. The signal itself is already
		// committed at this point, but the issue requires not reporting clean
		// completion after losing final persisted state, so a shutdown
		// failure here downgrades an otherwise-successful result to failure;
		// it never masks a run-outcome exit code that is already non-zero.
		if err := setup.Shutdown(context.Background()); err != nil {
			pf(stderr, "error: shut down scheduler services: %v\n", err)
			if result == 0 {
				result = 1
			}
		}
	}()
	if err := claimRecovery.finish(ctx, l, setup, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	service, err := installOneShotSignalQueue(l, setup)
	if err != nil {
		pf(stderr, "error: initialize signal custody: %v\n", err)
		return 1
	}
	defer func() { _ = service.queue.Close() }()
	key := *requestID
	if key == "" {
		key, err = newRemoteTriggerRequestID()
		if err != nil {
			pf(stderr, "error: allocate signal key: %v\n", err)
			return 1
		}
	}
	pf(stderr, "signal request-id: %s\n", key)
	opts := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(setup.RunConditions.MaxParallelRuns, setup.RunConditions.WorkflowBudgets, setup.RunConditions.WorkflowDailyBudgets))
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, opts...)
	defer func() { sched.Wait(); wg.Wait() }()
	runDirs, err := l.RunDirs()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := sched.ReconcileAll(runDirs, time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	runIDs, pending, err := dispatchQueuedSignal(ctx, service, sched, key, name, stdout)
	if err != nil {
		pf(stderr, "error: signal request %q: %v\n", key, err)
		return 1
	}
	if len(runIDs) == 0 {
		if pending {
			return 1
		}
		pf(stdout, "signal %q delivered: no subscribed workflow was admitted (none subscribed, or run conditions rejected every match)\n", name)
		return 0
	}
	for _, runID := range runIDs {
		pf(stdout, "created run %s (signal=%s)\n", runID, name)
	}

	// Wait for every dispatched run to reach a terminal state, same as
	// `goobers run` — required, not just nicer UX. Scheduler.dispatch
	// registers each dispatch with the wait group before launching its
	// goroutine, and the tracked starter keeps that registration until all
	// post-run telemetry has completed. waitForRunTerminal's polling loop
	// observes each run's journal while the wait group is reserved for the
	// final drain below.
	type waitResult struct {
		index int
		runID string
		phase journal.RunPhase
		err   error
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	results := make(chan waitResult, len(runIDs))
	progress := &synchronizedWriter{out: stderr}
	for index, runID := range runIDs {
		index := index
		runID := runID
		go func() {
			phase, err := waitForRunTerminalInLayoutWithProgress(waitCtx, l, runID, progress)
			results <- waitResult{index: index, runID: runID, phase: phase, err: err}
		}()
	}

	exitCode := 0
	var waitErr error
	completed := make([]waitResult, len(runIDs))
	ready := make([]bool, len(runIDs))
	next := 0
	for range runIDs {
		result := <-results
		if result.err != nil {
			if waitErr == nil {
				waitErr = result.err
				cancelWait()
			}
			continue
		}
		if waitErr != nil {
			continue
		}
		completed[result.index] = result
		ready[result.index] = true
		for next < len(runIDs) && ready[next] {
			result = completed[next]
			pf(stdout, "finished: run=%s phase=%s\n", result.runID, result.phase)
			if phaseExit := exitForPhase(result.phase); phaseExit > exitCode {
				exitCode = phaseExit
			}
			next++
		}
	}
	if waitErr != nil {
		pf(stderr, "error: %v\n", waitErr)
		return 2
	}
	wg.Wait()
	pf(stdout, "inspect with: goobers trace <run-id> %s\n", root)
	if pending && exitCode == 0 {
		return 1
	}
	return exitCode
}
