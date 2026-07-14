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

// runPollInterval bounds how often waitForRunTerminal re-reads a run's
// journal while `goobers run` blocks on it. Var, not const, so tests don't
// have to wait out a real 200ms per poll.
var runPollInterval = 200 * time.Millisecond

func runRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers run <workflow> [path]\n\n"+
			"Trigger a run of a config/ workflow manually, through the same scheduler\n"+
			"(run conditions, instance journal, single-instance lock) a live `goobers up`\n"+
			"daemon uses, then wait for it to reach a terminal state or pause (default\n"+
			"path \".\"). Exit codes: 0 = run created and dispatched, 1 = business error\n"+
			"(unknown workflow, invalid config, run conditions rejected the trigger, a\n"+
			"daemon already holds this instance's lock), 2 = usage/IO error.\n")
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

	// Take the same single-instance lock `up` does (issue #134): a manual run
	// must not mutate scheduler/run-condition/claim-ledger state, or the
	// shared workcopies/ tree, concurrently with a live daemon. Handing off to
	// an already-running daemon (rather than failing here) is a known
	// follow-up — no IPC/API surface exists yet for a short-lived `run`
	// process to delegate to a long-running `up` process.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v (a running `goobers up` daemon holds this instance's lock — "+
			"trigger workflows through it, or stop it first)\n", err)
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
	defer func() { _ = setup.Telemetry.Shutdown(context.Background()) }()

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

	sched := localscheduler.New(setup.Entries, setup.InstanceLog, localscheduler.WithTelemetry(setup.Telemetry))
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	runID, err := sched.Trigger(ctx, name, time.Now())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "created run %s (workflow=%s gaggle=%s)\n", runID, name, gaggle)

	phase, err := waitForRunTerminal(ctx, l.RunsDir(), runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	pf(stdout, "finished: phase=%s\n", phase)
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
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
