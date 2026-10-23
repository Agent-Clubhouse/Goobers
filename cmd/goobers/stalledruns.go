package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type stalledSweepDeps = scheduling.StalledSweepDeps

var cleanDaemonDowntime = scheduling.CleanDaemonDowntime

func stalledServices() scheduling.StallServices {
	return scheduling.StallServices{Candidates: recoveryRunCandidates, CleanupGuard: mutationCleanupGuard, FinalizeTerminal: finalizeTerminalRun, Clock: scheduling.JournalActivityClock{}}
}
func sweepStalledRuns(
	ctx context.Context,
	l instance.Layout,
	runners *daemonRunnerRegistry,
	fallback *runner.Runner,
	log *journal.InstanceLog,
	deps *stalledSweepDeps,
	notify runner.TerminalNotifier,
	release func(runID, workflow string),
	now time.Time,
	timeout time.Duration,
	maxDuration time.Duration,
	recoveryRunDirs ...[]string,
) error {
	return stalledServices().Sweep(ctx, l, runners, fallback, log, deps, notify, release, now, timeout, maxDuration, recoveryRunDirs...)
}
