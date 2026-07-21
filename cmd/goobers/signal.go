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

// runSignal implements `goobers signal <name>` (#342): fires an external
// signal by name, dispatching every workflow with a type=signal trigger
// subscribed to it. TriggerSignal was declared in the schema
// (api/v1alpha1.TriggerSignal) but compiled and dispatched nowhere before
// this — this is the first real delivery mechanism for it, mirroring
// `goobers run <workflow>`'s manual-trigger CLI wiring. An HTTP/webhook sink
// (#169, once the daemon has a write-capable API surface) is the planned
// future caller of Scheduler.Signal; this CLI path has no opinion on
// delivery mechanism and works standalone in the meantime.
func runSignal(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("signal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers signal <name> [path]\n\n"+
			"Fire an external signal by name, dispatching every workflow with a\n"+
			"type=signal trigger subscribed to it, through the same scheduler (run\n"+
			"conditions, instance journal, single-instance lock) a live `goobers up`\n"+
			"daemon uses (default path \".\"). A signal may match zero, one, or many\n"+
			"workflows; waits for every dispatched run to reach a terminal state or\n"+
			"pause before returning (same blocking UX as `goobers run`).\n"+
			"Exit codes after waiting: 0 = every admitted run completed (also used when\n"+
			"none were admitted), 1 = any run failed/aborted or a business error, 2 =\n"+
			"usage/IO error, 3 = any run escalated. Escalation takes precedence for\n"+
			"mixed outcomes; successful submission-only modes exit 0 because they do\n"+
			"not observe a terminal phase.\n")
	}
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
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}

	// Same single-instance lock `up`/`run` take (issue #134): a manual signal
	// must not mutate scheduler/run-condition/claim-ledger state concurrently
	// with a live daemon. Handing off to an already-running daemon is #343's
	// gap, not this command's — same known limitation `goobers run` already
	// documents.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v (a running `goobers up` daemon holds this instance's lock — "+
			"stop it first; `goobers up` has no live workflow-trigger delegation yet, "+
			"see the doc comment on cmd/goobers/run.go's lock-acquire step)\n", err)
		return 1
	}
	defer release()

	ctx, stop := signals.SetupSignalContext()
	defer stop()

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer setup.Shutdown(context.Background())

	opts := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(setup.RunConditions.MaxParallelRuns, setup.RunConditions.WorkflowBudgets, setup.RunConditions.WorkflowDailyBudgets))
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, opts...)
	runDirs, err := l.RunDirs()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := sched.ReconcileAll(runDirs, time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	runIDs := sched.Signal(ctx, name, time.Now())
	if len(runIDs) == 0 {
		pf(stdout, "signal %q delivered: no subscribed workflow was admitted (none subscribed, or run conditions rejected every match)\n", name)
		return 0
	}
	for _, runID := range runIDs {
		pf(stdout, "created run %s (signal=%s)\n", runID, name)
	}

	// Wait for every dispatched run to reach a terminal state, same as
	// `goobers run` — required, not just nicer UX: dispatch's goroutine calls
	// wg.Add(1) from inside trackedStarter.Start, asynchronously relative to
	// Signal's return, so a bare wg.Wait() here would race it (Wait can
	// observe the counter still at 0 and return immediately, before the run
	// even started) — the same Add-before-Wait requirement sync.WaitGroup
	// always has. waitForRunTerminal's polling loop naturally closes that
	// race by blocking until each run's own journal shows it under way.
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
	return exitCode
}
