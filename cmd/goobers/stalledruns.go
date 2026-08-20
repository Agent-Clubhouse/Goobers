package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/boundedagg"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type stalledTerminalPreparer func(instance.Layout) (runner.TerminalPreparer, error)

// daemonRunnerRegistry retains each live run's owning Runner while atomically
// swapping the configured fallback runners during config reload.
type daemonRunnerRegistry struct {
	mu             sync.RWMutex
	current        map[string]*runner.Runner
	owners         map[string]trackedRun
	nextGeneration uint64
	hardStopping   bool
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
	if r == nil || owner == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.owners == nil {
		r.owners = make(map[string]trackedRun)
	}
	lease := r.owners[runID]
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
	}
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
	if r == nil || owner == nil {
		return func() {}, false
	}
	r.mu.Lock()
	if r.owners == nil {
		r.owners = make(map[string]trackedRun)
	}
	lease := r.owners[runID]
	if lease.owner != nil && lease.owner != owner {
		r.mu.Unlock()
		return func() {}, false
	}
	if lease.owner == owner {
		lease.leases++
	} else {
		r.nextGeneration++
		lease = trackedRun{RunID: runID, owner: owner, generation: r.nextGeneration, leases: 1}
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

func sweepStalledRuns(
	l instance.Layout,
	runners *daemonRunnerRegistry,
	fallback *runner.Runner,
	log *journal.InstanceLog,
	prepare stalledTerminalPreparer,
	notify runner.TerminalNotifier,
	release func(runID, workflow string),
	now time.Time,
	timeout time.Duration,
	maxDuration time.Duration,
) error {
	runDirs, err := l.RunDirs()
	if err != nil {
		return err
	}

	// Accumulate per-entry failures into a slice, then bound the aggregate at
	// the end: a sweep over a pathological number of orphan/bad run directories
	// must never build an unbounded error message that then bloats the
	// scheduler journal when persisted (#1166, #1414).
	var sweepErrs []error
	terminalizers := make(map[string]*runner.Runner)
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("read runs directory %s: %w", runsDir, err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runDir := filepath.Join(runsDir, entry.Name())
			reader, err := journal.OpenRead(runDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				sweepErrs = append(sweepErrs, fmt.Errorf("inspect run directory %q: %w", entry.Name(), err))
				continue
			}
			identity, err := reader.Identity()
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("read run %q identity: %w", entry.Name(), err))
				continue
			}
			runTimeout := timeout
			runMaxDuration := maxDuration
			if identity.RunControls != nil {
				if controlsErr := runcontrol.ValidatePinned(identity.RunControls); controlsErr != nil {
					sweepErrs = append(sweepErrs, fmt.Errorf("read run %q controls: %w", identity.RunID, controlsErr))
					continue
				}
				runTimeout, _ = time.ParseDuration(identity.RunControls.StalledRunTimeout)
				runMaxDuration = 0
				if identity.RunControls.MaxRunDuration != "" {
					runMaxDuration, _ = time.ParseDuration(identity.RunControls.MaxRunDuration)
				}
			}
			phase, err := reader.Phase()
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("read run %q phase: %w", identity.RunID, err))
				continue
			}
			if phase != journal.PhaseRunning {
				continue
			}
			durationExceeded := runMaxDuration > 0 && identity.StartedAt.Before(now.Add(-runMaxDuration))
			if !durationExceeded {
				events, eventsErr := reader.Events()
				if eventsErr != nil {
					sweepErrs = append(sweepErrs, fmt.Errorf("read run %q events: %w", identity.RunID, eventsErr))
					continue
				}
				if len(events) == 0 {
					sweepErrs = append(sweepErrs, fmt.Errorf("running run %q has no journal events", identity.RunID))
					continue
				}
				if events[len(events)-1].Type == journal.EventGatePaused {
					continue
				}
				if !events[len(events)-1].Time.Before(now.Add(-runTimeout)) {
					continue
				}
			}

			runLayout := l
			if filepath.Clean(runsDir) != filepath.Clean(l.RunsDir()) {
				rootGaggle := filepath.Base(filepath.Dir(runsDir))
				runLayout = l.ForGaggle(rootGaggle)
			}
			runRunner, liveOwner := runners.Resolve(identity.RunID, runLayout.Gaggle(), fallback)
			if runRunner == nil {
				runRunner = terminalizers[runsDir]
				if runRunner == nil {
					var terminalPreparer runner.TerminalPreparer
					if prepare != nil {
						terminalPreparer, err = prepare(runLayout)
						if err != nil {
							sweepErrs = append(sweepErrs, fmt.Errorf("construct stalled-run terminal preparer for %s: %w", runsDir, err))
							continue
						}
					}
					manager, managerErr := worktree.NewManager(runLayout.WorkcopiesDir())
					if managerErr != nil {
						sweepErrs = append(sweepErrs, fmt.Errorf("construct stalled-run worktree manager for %s: %w", runsDir, managerErr))
						continue
					}
					runRunner, err = runner.New(runner.Config{
						Worktrees:       manager,
						RunsDir:         runsDir,
						PrepareTerminal: terminalPreparer,
						FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
							return finalizeTerminalRun(runLayout, log, manager, runID)
						},
						NotifyTerminal: notify,
					})
					if err != nil {
						sweepErrs = append(sweepErrs, fmt.Errorf("construct stalled-run terminalizer for %s: %w", runsDir, err))
						continue
					}
					terminalizers[runsDir] = runRunner
				}
			}
			var result runner.Result
			var terminated bool
			if durationExceeded {
				result, terminated, err = runRunner.ExpireRun(identity.RunID, now, identity.StartedAt, runMaxDuration)
			} else {
				result, terminated, err = runRunner.EscalateStalled(identity.RunID, now, runTimeout)
			}
			if terminated {
				if release != nil {
					release(identity.RunID, identity.Workflow)
				}
				if log != nil && !liveOwner {
					terminalPhase := journal.PhaseEscalated
					errorCode := runner.RunStalledErrorCode
					message := fmt.Sprintf("run exceeded %s without journal activity", runTimeout)
					if durationExceeded {
						terminalPhase = journal.PhaseAborted
						errorCode = runner.RunDurationExceededErrorCode
						message = fmt.Sprintf("run exceeded maximum duration %s", runMaxDuration)
					}
					appendErr := log.Append(journal.Event{
						Type:     journal.EventRunFinished,
						Gaggle:   identity.Gaggle,
						Workflow: identity.Workflow,
						RunID:    identity.RunID,
						Status:   string(terminalPhase),
						Error: &journal.ErrorDetail{
							Code:    errorCode,
							Message: message,
						},
					})
					err = errors.Join(err, appendErr)
				}
			}
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("terminate watchdog run %q (%s): %w", identity.RunID, result.Phase, err))
			}
		}
	}
	return boundedagg.Join(sweepErrs...)
}
