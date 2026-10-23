package main

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runtimewiring"
)

type stalledSweepDeps = scheduling.StalledSweepDeps

var cleanDaemonDowntime = scheduling.CleanDaemonDowntime

func stalledServices() scheduling.StallServices {
	return scheduling.StallServices{Candidates: recoveryRunCandidates, CleanupGuard: runtimewiring.MutationCleanupGuard, FinalizeTerminal: finalizeTerminalRun, Clock: scheduling.JournalActivityClock{}}
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

// daemonRunnerRegistry retains each live run's owning Runner while atomically
// swapping the configured fallback runners during config reload.
type daemonRunnerRegistry struct {
	reconcileContained     func(context.Context, journal.RunIdentity) error
	childCustody           map[string]chan struct{}
	resolveGeneration      executionGenerationResolver
	resolveChildGeneration executionGenerationResolver
	mu                     sync.RWMutex
	current                map[string]*runner.Runner
	owners                 map[string]trackedRun
	nextGeneration         uint64
	hardStopping           bool
}

func newDaemonRunnerRegistry() *daemonRunnerRegistry {
	return &daemonRunnerRegistry{owners: make(map[string]trackedRun)}
}

// trackedRun is a lease on a live run's owning Runner. generation/leases make
// Track/TrackCompatible reentrant-safe: concurrent trackers of the same run
// share one lease, and untracking only deletes the entry once every tracker
// bracketing that same generation has released it (a stale untrack closure
// from a superseded generation is a no-op).
type trackedRun struct {
	RunID      string
	Workflow   string
	owner      *runner.Runner
	generation uint64
	leases     int
}

func (r *daemonRunnerRegistry) Replace(current map[string]*runner.Runner) {
	if r == nil {
		return
	}
	replacement := make(map[string]*runner.Runner, len(current))
	for gaggle, rn := range current {
		replacement[gaggle] = rn
	}
	r.mu.Lock()
	r.current = replacement
	r.mu.Unlock()
}

func (r *daemonRunnerRegistry) Track(runID, workflow string, owner *runner.Runner) func() {
	release, _ := r.trackRunLease(runID, workflow, owner, false)
	return release
}

func (r *daemonRunnerRegistry) trackRunLease(runID, workflow string, owner *runner.Runner, requireCompatible bool) (func(), bool) {
	if r == nil || owner == nil {
		return func() {}, false
	}
	if !r.lockRunTracking(runID, requireCompatible) {
		return func() {}, false
	}
	if r.owners == nil {
		r.owners = make(map[string]trackedRun)
	}
	lease := r.owners[runID]
	if requireCompatible && lease.owner != nil && lease.owner != owner {
		r.mu.Unlock()
		return func() {}, false
	}
	if lease.owner == owner {
		lease.leases++
	} else {
		r.nextGeneration++
		lease = trackedRun{RunID: runID, Workflow: workflow, owner: owner, generation: r.nextGeneration, leases: 1}
	}
	r.owners[runID] = lease
	hardStopping := r.hardStopping
	r.mu.Unlock()
	if hardStopping {
		owner.HardStopRunWhenStarted(runID)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			current := r.owners[runID]
			if current.generation == lease.generation {
				current.leases--
				if current.leases == 0 {
					delete(r.owners, runID)
				} else {
					r.owners[runID] = current
				}
			}
			r.mu.Unlock()
		})
	}, true
}

// RunIDs lists every run this process is currently tracking — the in-process
// liveness signal issue #2014's claim-lease renewal uses instead of a
// per-stage heartbeat crossing into the claim ledger: a runID appears here
// exactly while this process is the one actively driving it (Track/untrack
// bracket Start/Resume), so a process that crashes or a run that finishes
// stops appearing without anything needing to notice and say so explicitly.
func (r *daemonRunnerRegistry) RunIDs() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.owners))
	for _, run := range r.owners {
		ids = append(ids, run.RunID)
	}
	sort.Strings(ids)
	return ids
}

// TrackCompatible is Track's reentrant-safe counterpart for the intervention
// path: it attaches only if the run is untracked or already owned by owner,
// so an in-flight intervention can never steal or clobber another tracker's
// lease. Track's own hardStopping propagation applies here too, since a run
// that becomes reachable mid-shutdown must still be stopped.
func (r *daemonRunnerRegistry) TrackCompatible(runID string, owner *runner.Runner) (func(), bool) {
	return r.trackRunLease(runID, "", owner, true)
}

func (r *daemonRunnerRegistry) ActiveRuns() []trackedRun {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	runs := make([]trackedRun, 0, len(r.owners))
	for _, run := range r.owners {
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID < runs[j].RunID })
	return runs
}

// HardStopAll invokes report while registration is blocked, immediately before
// stopping the runs counted for that report. report must not call the registry.
func (r *daemonRunnerRegistry) HardStopAll(report func(int)) int {
	if r == nil {
		if report != nil {
			report(0)
		}
		return 0
	}
	r.mu.Lock()
	r.hardStopping = true
	runs := make([]trackedRun, 0, len(r.owners))
	for _, run := range r.owners {
		runs = append(runs, run)
	}
	if report != nil {
		report(len(runs))
	}
	r.mu.Unlock()
	for _, run := range runs {
		run.owner.HardStopRunWhenStarted(run.RunID)
	}
	return len(runs)
}

func (r *daemonRunnerRegistry) Resolve(runID, gaggle string, fallback *runner.Runner) (*runner.Runner, bool) {
	if r == nil {
		return fallback, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if tracked, ok := r.owners[runID]; ok && tracked.owner != nil {
		return tracked.owner, true
	}
	if gaggle != "" {
		return r.current[gaggle], false
	}
	return fallback, false
}
