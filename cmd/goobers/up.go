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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// drainGrace bounds how long runUpContext waits, after its context is
// cancelled, for in-flight Start/Resume goroutines to checkpoint and return
// before exiting anyway (issue #23 AC: graceful drain, not an indefinite
// hang if a stage is wedged). Var, not const, so tests can shrink it rather
// than waiting out a real 30s.
var drainGrace = 30 * time.Second

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

	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		pf(stderr, "error: config directory invalid: %v\n", err)
		return 1
	}

	goobers := goobersByName(set)
	machines, err := compiledMachines(set, goobers)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	repoRefs, err := repoRefsByWorkflow(set)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	tel, err := buildTelemetryClient(ctx, l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()

	rollupDB, err := rollup.Open(l.TelemetryDB())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer func() { _ = rollupDB.Close() }()

	runnerCfg, err := buildRunnerConfig(l, cfg, goobers, tel)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	rn, err := runner.New(runnerCfg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}

	var wg sync.WaitGroup

	// Crash-resume: any run left non-terminal by a prior crash or unclean
	// shutdown restarts now, before the scheduler starts admitting new ticks
	// (#23 AC: restart via Runner.Resume).
	resumed, err := resumeInterruptedRuns(ctx, l, rn, machines, repoRefs, instanceLog, rollupDB, &wg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	for _, runID := range resumed {
		pf(stdout, "resuming interrupted run %s\n", runID)
	}

	entries := make([]localscheduler.WorkflowEntry, 0, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		var sched localscheduler.Schedule
		for _, tr := range wf.Spec.Triggers {
			if tr.Type == apiv1.TriggerSchedule && tr.Schedule != "" {
				s, err := localscheduler.ParseSchedule(tr.Schedule)
				if err != nil {
					pf(stderr, "error: workflow %q: %v\n", wf.Name, err)
					return 1
				}
				sched = s
				break
			}
		}
		entries = append(entries, localscheduler.WorkflowEntry{
			Workflow:  wf.Name,
			Gaggle:    wf.Spec.Gaggle,
			Readiness: wf.Spec.Readiness,
			Schedule:  sched,
			Starter:   &trackedStarter{r: rn, machine: machines[wf.Name], wg: &wg, l: l, rollupDB: rollupDB},
			RepoRef:   repoRefs[wf.Name],
		})
	}

	sched := localscheduler.New(entries, instanceLog, localscheduler.WithTelemetry(tel))
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	pf(stdout, "daemon started at %s (%d workflow(s))\n", root, len(entries))
	runErr := sched.Run(ctx) // blocks until ctx is cancelled
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
