package main

import (
	"context"
	"flag"
	"io"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

const engineStartHelp = "Usage: goobers engine-start [flags] <workflow> [path]\n\n" +
	"Dispatch one run onto the tier-3 engine (experimental).\n\n" +
	"When a `goobers up` daemon holds this instance's lock the dispatch is\n" +
	"DELEGATED to it: the daemon admits the run through the scheduler (so it\n" +
	"takes a concurrency slot, records an instance-log run.started, reserves\n" +
	"the run journal and fires the terminal hooks on completion) and starts\n" +
	"the workflow through its own engine starter. The run id is the\n" +
	"scheduler's, and --dedupe-key is refused, because the daemon mints a\n" +
	"fresh run id per admission.\n\n" +
	"--direct bypasses the daemon and starts the workflow straight on\n" +
	"Temporal with REJECT_DUPLICATE, deriving the run id from gaggle,\n" +
	"workflow and --dedupe-key. That is the only mode in which --dedupe-key\n" +
	"means anything: a direct start's run id IS its dedupe unit, whereas a\n" +
	"delegated dispatch dedupes DELIVERIES (by request id) and not work.\n" +
	"A direct start takes no scheduler slot and fires no terminal hooks.\n" +
	"--direct is implied when no daemon is running.\n\n" +
	"--live-journal pins live journal authorship into the run: workers emit\n" +
	"journal events through the daemon's journal plane as they happen, so the\n" +
	"run is visible mid-flight; without it the journal is projected from\n" +
	"history at close, as before. Requires the daemon's write API to be\n" +
	"reachable from every worker serving the run (worker --daemon-api).\n\n" +
	"Exit codes: 0 = started, 1 = dispatch failure, 2 = usage/config error.\n"

func runEngineStart(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("engine-start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the workflow")
	hostPort := fs.String("temporal-hostport", "", "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", "", "Temporal namespace")
	taskQueue := fs.String("task-queue", "", "task queue to dispatch onto")
	dedupe := fs.String("dedupe-key", "", "dedupe key used to derive the run id")
	liveJournal := fs.Bool("live-journal", false, "author the run journal live through the daemon's journal plane (DS4)")
	direct := fs.Bool("direct", false, "start the workflow straight on Temporal, bypassing the daemon's scheduler")
	fs.Usage = helpUsage(stderr, "engine-start")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		pf(stderr, "usage: goobers engine-start [flags] <workflow> [path]\n")
		return 2
	}
	workflowName := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineStartTimeout)
	defer cancel()

	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 2
	}
	engineConfig := cfg.EffectiveEngineConfig()
	if *hostPort == "" {
		*hostPort = engineConfig.HostPort
	}
	if *namespace == "" {
		*namespace = engineConfig.Namespace
	}
	if *taskQueue == "" {
		*taskQueue = engineConfig.TaskQueue
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		printValidationIssues(stderr, report)
		pf(stderr, "error: load config directory: %v\n", err)
		return 2
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	target := *gaggle
	if target == "" {
		for i := range set.Workflows {
			if set.Workflows[i].Name == workflowName {
				target = set.Workflows[i].Spec.Gaggle
				break
			}
		}
	}
	if target == "" {
		pf(stderr, "error: workflow %q not found; pass --gaggle to disambiguate\n", workflowName)
		return 2
	}

	reg, project, err := bootstrap.RegisterGaggleWorkflows(set, target)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	def, ok := reg.Latest(workflowName)
	if !ok {
		pf(stderr, "error: workflow %q is not registered\n", workflowName)
		return 1
	}
	// Delegation decision, before anything is pinned: a live daemon owns the
	// scheduler, and a direct start behind its back takes no concurrency
	// slot, writes no instance-log run.started, and fires none of the
	// terminal hooks that release claims and record the circuit breaker. That
	// is the #343 rule `goobers run` has followed for the runner; decision
	// 005 D1 extends it to the engine now that the daemon can start engine
	// runs itself.
	release, lockErr := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	daemonUp := lockErr != nil
	if release != nil {
		release()
	}
	if daemonUp && !*direct {
		if *dedupe != "" {
			// Refused, not silently ignored. The trigger plane's RequestID
			// dedupes DELIVERIES — it stops one request being dispatched
			// twice — and the scheduler mints a fresh run id on every
			// admission, so there is no unit-of-work identity for a dedupe
			// key to name on this path. Accepting the flag and dropping it
			// would let an operator believe two dispatches had collapsed
			// into one run when they had not.
			pf(stderr, "error: --dedupe-key requires --direct; a daemon-delegated dispatch dedupes deliveries by request id, not work, and the scheduler mints a fresh run id per admission\n")
			return 2
		}
		return delegateEngineStart(ctx, l, target, workflowName, stdout, stderr)
	}
	if !daemonUp && !*direct {
		pf(stderr, "note: no daemon holds %s; starting directly on Temporal (no scheduler slot, no terminal hooks)\n", l.SchedulerDir())
	}

	spec, err := engineRunSpec(engineRunRequest{
		cfg:         cfg,
		set:         set,
		gaggle:      target,
		dedupeKey:   *dedupe,
		project:     project,
		def:         def,
		liveJournal: *liveJournal,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	in, err := reg.StartInput(workflowName, spec)
	if err != nil {
		pf(stderr, "error: pin workflow %q: %v\n", workflowName, err)
		return 1
	}

	c, err := client.DialContext(ctx, client.Options{HostPort: *hostPort, Namespace: *namespace})
	if err != nil {
		pf(stderr, "error: dial temporal at %s: %v\n", *hostPort, err)
		return 1
	}
	defer c.Close()
	res, err := engine.NewTemporalStarter(c, *taskQueue).Start(ctx, in)
	if err != nil {
		pf(stderr, "error: start engine run: %v\n", err)
		return 1
	}
	pf(stdout, "engine run started: %s (workflow=%s v%d, gaggle=%s, queue=%s)\n", res.RunID, in.WorkflowName, in.Version, in.Gaggle, *taskQueue)
	return 0
}

// engineStartTimeout bounds the whole dispatch — the Temporal dial and start
// on the direct path, and the delegation round trip on the daemon path.
const engineStartTimeout = 60 * time.Second

// delegateEngineStart hands one engine dispatch to the live daemon through
// the pending-trigger file the daemon's sweep already serves, and reports the
// run id the daemon admitted.
//
// It reuses `goobers run`'s delegation channel verbatim rather than adding an
// engine-specific one, because after decision 005 D1 the daemon's per-entry
// Starter selection is what decides whether a lane runs on the engine or the
// local runner. An engine-specific delegation channel would have to make that
// decision a second time, in a second place, from a CLI process that does not
// hold the runner inventory the daemon resolved at boot — and the two answers
// would drift the moment a lane's pins changed. Asking the daemon to trigger
// the workflow means the lane's CURRENT selection applies, and this command
// reports which starter actually ran.
func delegateEngineStart(ctx context.Context, l instance.Layout, gaggle, workflowName string, stdout, stderr io.Writer) int {
	requestID, err := writeTriggerRequestContext(ctx, l.SchedulerDir(), gaggle, workflowName)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	runID, err := pollTriggerResponse(ctx, l.SchedulerDir(), requestID, triggerDelegationTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "run dispatched via live daemon: %s (workflow=%s, gaggle=%s)\n", runID, workflowName, gaggle)
	pf(stdout, "inspect with: goobers trace %s\n", runID)
	return 0
}
