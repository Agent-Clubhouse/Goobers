package main

import (
	"context"
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

// stalledSweepDeps carries the daemon-owned wiring the sweep needs when it has
// to build its OWN terminalizer — the case where no live Runner owns the run,
// which is every run left behind by a previous daemon.
//
// A struct rather than two more positional parameters: both fields come from
// the same daemon setup, both are absent outside `up`, and this argument list
// is already long enough that one more bare `nil` at a call site would say
// nothing about what was being omitted. A nil *stalledSweepDeps is the
// no-daemon case and behaves exactly as passing no preparer did.
type stalledSweepDeps struct {
	// PrepareTerminal builds the external forge cleanup for a run's gaggle.
	PrepareTerminal stalledTerminalPreparer
	// JournalAdvanced is the read-model intake observer (#5278). Without it a
	// run this sweep terminalizes appends run.finished with nobody watching:
	// no intake watermark is recorded, so the projector never re-reads the
	// run, and repair only DISCOVERS unprojected runs — it never refreshes one
	// it already has. The row stays `running` forever while the journal says
	// otherwise, which is what manufactured four "stuck for weeks" runs on the
	// cloud instance and hid the genuinely stalled ones among them.
	JournalAdvanced        func(runID string, seq uint64)
	JournalAdvancedContext func(context.Context, string, uint64)
	// DrainedDowntime is every interval the daemon was down after a graceful
	// drain (#5601), read once at startup by cleanDaemonDowntime. A drained
	// run is parked at a stage boundary on purpose and nothing can progress it
	// until the next daemon starts, so that interval is not the run's own
	// inactivity: the stall check extends each run's timeout by the part of
	// these intervals that falls after its last activity. Time the daemon was
	// up, and downtime after a crash or a forced drain, still count.
	DrainedDowntime []daemonDowntime
}

func (d *stalledSweepDeps) prepareTerminal() stalledTerminalPreparer {
	if d == nil {
		return nil
	}
	return d.PrepareTerminal
}

func (d *stalledSweepDeps) journalAdvancedContext() func(context.Context, string, uint64) {
	if d == nil {
		return nil
	}
	return d.JournalAdvancedContext
}

func (d *stalledSweepDeps) journalAdvanced() func(string, uint64) {
	if d == nil {
		return nil
	}
	return d.JournalAdvanced
}

// drainedDowntimeSince returns how much graceful-drain downtime falls after
// lastActivity. A zero lastActivity credits every interval; the runner still
// refuses to escalate a run whose activity it cannot date.
func (d *stalledSweepDeps) drainedDowntimeSince(lastActivity time.Time) time.Duration {
	if d == nil {
		return 0
	}
	var total time.Duration
	for _, window := range d.DrainedDowntime {
		from := window.from
		if from.Before(lastActivity) {
			from = lastActivity
		}
		if window.to.After(from) {
			total += window.to.Sub(from)
		}
	}
	return total
}

// daemonDowntime is one interval between a daemon.clean_shutdown and the next
// daemon.started.
type daemonDowntime struct {
	from, to time.Time
}

// cleanDaemonDowntime pairs each clean shutdown with the daemon start that
// follows it. A dirty restart, or a start with no clean shutdown before it,
// contributes nothing: when a crashed daemon stopped is unknown, so that gap
// keeps counting toward the stall timeout exactly as before.
func cleanDaemonDowntime(events []journal.Event) []daemonDowntime {
	var windows []daemonDowntime
	var shutdownAt time.Time
	for _, event := range events {
		switch event.Type {
		case journal.EventDaemonCleanShutdown:
			shutdownAt = event.Time
		case journal.EventDaemonStarted:
			if !shutdownAt.IsZero() && event.Time.After(shutdownAt) {
				windows = append(windows, daemonDowntime{from: shutdownAt, to: event.Time})
			}
			shutdownAt = time.Time{}
		case journal.EventDaemonDirtyRestart:
			shutdownAt = time.Time{}
		}
	}
	return windows
}

// daemonRunnerRegistry retains each live run's owning Runner while atomically
// swapping the configured fallback runners during config reload.
type daemonRunnerRegistry struct {
	childCustody                 map[string]chan struct{}
	reconcileContained           func(context.Context, journal.RunIdentity) error
	resolveGeneration            executionGenerationResolver
	resolveChildGeneration       executionGenerationResolver
	resolveInteractiveGeneration executionGenerationResolver
	mu                           sync.RWMutex
	current                      map[string]*runner.Runner
	owners                       map[string]trackedRun
	nextGeneration               uint64
	hardStopping                 bool
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

// newStalledTerminalizer builds the Runner the sweep uses to terminalize runs
// under runsDir that no live Runner owns — every run left behind by a previous
// daemon, plus any whose owner has already gone away.
//
// One per runs directory, cached by the caller: the construction cost is per
// gaggle, not per run.
func newStalledTerminalizer(
	runLayout instance.Layout,
	runsDir string,
	log *journal.InstanceLog,
	deps *stalledSweepDeps,
	notify runner.TerminalNotifier,
) (*runner.Runner, error) {
	var terminalPreparer runner.TerminalPreparer
	if prepare := deps.prepareTerminal(); prepare != nil {
		prepared, err := prepare(runLayout)
		if err != nil {
			return nil, fmt.Errorf("construct stalled-run terminal preparer for %s: %w", runsDir, err)
		}
		terminalPreparer = prepared
	}
	manager, err := worktree.NewManager(runLayout.WorkcopiesDir(), mutationCleanupGuard(runsDir))
	if err != nil {
		return nil, fmt.Errorf("construct stalled-run worktree manager for %s: %w", runsDir, err)
	}
	terminalizer, err := runner.New(runner.Config{
		Worktrees:       manager,
		RunsDir:         runsDir,
		PrepareTerminal: terminalPreparer,
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return finalizeTerminalRun(runLayout, log, manager, runID)
		},
		NotifyTerminal: notify,
		// Without this the run.finished this terminalizer appends is invisible
		// to every derived reader — see stalledSweepDeps.JournalAdvanced.
		JournalAdvanced:        deps.journalAdvanced(),
		JournalAdvancedContext: deps.journalAdvancedContext(),
	})
	if err != nil {
		return nil, fmt.Errorf("construct stalled-run terminalizer for %s: %w", runsDir, err)
	}
	return terminalizer, nil
}

// sweepStalledRuns terminalizes runs whose journals have gone quiet past
// their pinned stalledRunTimeout (or past maxRunDuration).
//
// An engine-driven run (journal.RunIdentity.EngineDriven) is cancelled on the
// engine instead, through guards. Terminalizing its journal file would settle
// nothing: the workflow keeps executing, keeps placing stages and keeps
// emitting into a journal this sweep just declared finished — nothing in the
// tree called CancelWorkflow before this change, so a stalled engine run's
// only outcome was a lie on disk. When no engine client exists the sweep
// refuses both options and reports it, because terminalizing is the failure
// mode, not the fallback.
//
// A cancel that itself fails is reported and retried on the next tick; it is
// NOT downgraded to terminalizing the file. That includes NotFound, which is
// not proof the run is over: a scheduled engine run's RunID is a hash of its
// claim workflow's id (internal/engine's RunScheduled), so NotFound is the
// normal answer for one that is executing perfectly well. Repeated identical
// failures are collapsed by up.go's sweepErrorReporter rather than journaled
// every tick.
func sweepStalledRuns(
	ctx context.Context,
	l instance.Layout,
	runners *daemonRunnerRegistry,
	fallback *runner.Runner,
	guards *engineRunGuards,
	log *journal.InstanceLog,
	deps *stalledSweepDeps,
	notify runner.TerminalNotifier,
	release func(runID, workflow string),
	now time.Time,
	timeout time.Duration,
	maxDuration time.Duration,
	recoveryRunDirs ...[]string,
) error {
	candidates, err := recoveryRunCandidates(ctx, l, recoveryRunDirs...)
	if err != nil {
		return err
	}

	// Accumulate per-entry failures into a slice, then bound the aggregate at
	// the end: a sweep over a pathological number of orphan/bad run directories
	// must never build an unbounded error message that then bloats the
	// scheduler journal when persisted (#1166, #1414).
	var sweepErrs []error
	terminalizers := make(map[string]*runner.Runner)
	for _, runDir := range candidates {
		runsDir := filepath.Dir(runDir)
		entryName := filepath.Base(runDir)
		reader, err := journal.OpenRead(runDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			sweepErrs = append(sweepErrs, fmt.Errorf("inspect run directory %q: %w", entryName, err))
			continue
		}
		identity, err := reader.Identity()
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("read run %q identity: %w", entryName, err))
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
		events, elapsed, elapsedErr := stalledExecutionClock(reader, identity.StartedAt, now)
		if elapsedErr != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("read run %q execution clock: %w", identity.RunID, elapsedErr))
			continue
		}
		durationExceeded := runMaxDuration > 0 && elapsed > runMaxDuration
		stallWindow := runTimeout
		if !durationExceeded {
			if len(events) == 0 {
				sweepErrs = append(sweepErrs, fmt.Errorf("running run %q has no journal events", identity.RunID))
				continue
			}
			// Parked at a gate is the sweep's one exemption, and it has to
			// hold even when something other than the runner appended after
			// the pause: a mode-3 pod emits into this journal through the
			// write API's journal plane (livejournal.Writer.Adopt), so a
			// retried emit or a pod-executed gate's own events can follow
			// gate.paused. Testing only the last event escalated a run that
			// was still waiting for a human. See journal.ParkedAtGate.
			if journal.ParkedAtGate(events) || runner.ParkedOnChild(events) {
				continue
			}
			lastActivity := events[len(events)-1].Time
			stallWindow = runTimeout + deps.drainedDowntimeSince(lastActivity)
			if !lastActivity.Before(now.Add(-stallWindow)) {
				continue
			}
		}

		// Past the timeout and engine-driven: cancel the workflow and
		// leave the journal alone. The engine writes the run's terminal
		// event itself once the cancellation lands — internal/engine's
		// cancel arm records run_failed + run.finished(aborted) through a
		// disconnected context, so the run closes out on the same journal
		// plane that has been authoring it all along. (Before that arm
		// existed a cancelled run had NO terminal, which would have left
		// this sweep cancelling a closed execution on every later tick.)
		//
		// The phase differs from the runner-driven neighbour below, which
		// this sweep escalates: the engine reports what actually happened
		// to its workflow, and what happened is a cancellation.
		if identity.EngineDriven() {
			if err := guards.cancel(ctx, identity.RunID); err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf("cancel stalled engine run %q: %w", identity.RunID, err))
				continue
			}
			if log != nil {
				message := fmt.Sprintf("run exceeded %s without journal activity", runTimeout)
				if durationExceeded {
					message = fmt.Sprintf("run exceeded maximum duration %s", runMaxDuration)
				}
				appendErr := log.Append(journal.Event{
					Type: journal.EventRunnerAnnotation, Gaggle: identity.Gaggle, Workflow: identity.Workflow, RunID: identity.RunID,
					Runner: map[string]any{
						"kind":   journal.RunnerAnnotationRunRecovery,
						"reason": message,
						"action": journal.RecoveryActionEngineCancelRequested,
						"driver": string(identity.Driver),
					},
				})
				if appendErr != nil {
					sweepErrs = append(sweepErrs, fmt.Errorf("journal engine cancel for run %q: %w", identity.RunID, appendErr))
				}
			}
			// Deliberately no `release`: a cancellation is a REQUEST, and
			// the run's scheduler slot belongs to whoever learns the
			// outcome. For a run this daemon seeded at startup that is
			// reattachEngineRun's goroutine, still waiting on the workflow
			// and releasing when it closes; a run started during this
			// daemon's life has no reconciled slot to release at all.
			// Freeing it here, on a request that has not landed yet, is the
			// same duplicate-admission hazard from the other end.
			continue
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
				runRunner, err = newStalledTerminalizer(runLayout, runsDir, log, deps, notify)
				if err != nil {
					sweepErrs = append(sweepErrs, err)
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
			result, terminated, err = runRunner.EscalateStalled(identity.RunID, now, stallWindow)
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
	return boundedagg.Join(sweepErrs...)
}

// Read once for both the active execution clock and the stall/parking checks.
func stalledExecutionClock(reader *journal.Reader, startedAt, now time.Time) ([]journal.Event, time.Duration, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, 0, err
	}
	elapsed, err := runner.RunExecutionElapsed(events, startedAt, now)
	return events, elapsed, err
}
