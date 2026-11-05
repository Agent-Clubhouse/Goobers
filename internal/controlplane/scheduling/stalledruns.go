package scheduling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/boundedagg"
	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/worktree"
)

// StalledTerminalPreparer constructs external cleanup for an orphaned run.
type StalledTerminalPreparer func(instance.Layout) (runner.TerminalPreparer, error)

// StalledSweepDeps carries the daemon-owned wiring the sweep needs when it has
// to build its OWN terminalizer — the case where no live Runner owns the run,
// which is every run left behind by a previous daemon.
//
// A struct rather than two more positional parameters: both fields come from
// the same daemon setup, both are absent outside `up`, and this argument list
// is already long enough that one more bare `nil` at a call site would say
// nothing about what was being omitted. A nil *StalledSweepDeps is the
// no-daemon case and behaves exactly as passing no preparer did.
type StalledSweepDeps struct {
	// PrepareTerminal builds the external forge cleanup for a run's gaggle.
	PrepareTerminal StalledTerminalPreparer
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
	// drain (#5601), read once at startup by CleanDaemonDowntime. A drained
	// run is parked at a stage boundary on purpose and nothing can progress it
	// until the next daemon starts, so that interval is not the run's own
	// inactivity: the stall check extends each run's timeout by the part of
	// these intervals that falls after its last activity. Time the daemon was
	// up, and downtime after a crash or a forced drain, still count.
	DrainedDowntime DowntimeSource
	// HostSuspensions is every interval the host was observed suspended
	// (#5891). maxRunDuration counts active execution time, and silence while
	// the host slept is not a stall, so both checks exclude these intervals.
	HostSuspensions SuspensionSource
}

func (d *StalledSweepDeps) hostSuspensions() []hostsuspend.Window {
	if d == nil {
		return nil
	}
	if d.HostSuspensions == nil {
		return nil
	}
	return d.HostSuspensions.Windows()
}

func (d *StalledSweepDeps) prepareTerminal() StalledTerminalPreparer {
	if d == nil {
		return nil
	}
	return d.PrepareTerminal
}

func (d *StalledSweepDeps) journalAdvancedContext() func(context.Context, string, uint64) {
	if d == nil {
		return nil
	}
	return d.JournalAdvancedContext
}

func (d *StalledSweepDeps) journalAdvanced() func(string, uint64) {
	if d == nil {
		return nil
	}
	return d.JournalAdvanced
}

// drainedDowntimeSince returns how much graceful-drain downtime falls after
// lastActivity. A zero lastActivity credits every interval; the runner still
// refuses to escalate a run whose activity it cannot date.
func (d *StalledSweepDeps) drainedDowntimeSince(lastActivity time.Time) time.Duration {
	if d == nil {
		return 0
	}
	if d.DrainedDowntime == nil {
		return 0
	}
	return d.DrainedDowntime.Since(lastActivity)
}

// DowntimeWindows is the immutable set of graceful shutdown intervals.
type DowntimeWindows []DaemonDowntime

// Since totals graceful downtime after the last observed activity.
func (windows DowntimeWindows) Since(lastActivity time.Time) time.Duration {
	var total time.Duration
	for _, window := range windows {
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

// DaemonDowntime is one interval between a daemon.clean_shutdown and the next
// daemon.started.
type DaemonDowntime struct {
	from, to time.Time
}

// CleanDaemonDowntime pairs each clean shutdown with the daemon start that
// follows it. A dirty restart, or a start with no clean shutdown before it,
// contributes nothing: when a crashed daemon stopped is unknown, so that gap
// keeps counting toward the stall timeout exactly as before.
func CleanDaemonDowntime(events []journal.Event) DowntimeWindows {
	var windows []DaemonDowntime
	var shutdownAt time.Time
	for _, event := range events {
		switch event.Type {
		case journal.EventDaemonCleanShutdown:
			shutdownAt = event.Time
		case journal.EventDaemonStarted:
			if !shutdownAt.IsZero() && event.Time.After(shutdownAt) {
				windows = append(windows, DaemonDowntime{from: shutdownAt, to: event.Time})
			}
			shutdownAt = time.Time{}
		case journal.EventDaemonDirtyRestart:
			shutdownAt = time.Time{}
		}
	}
	return windows
}

// newStalledTerminalizer builds the Runner the sweep uses to terminalize runs
// under runsDir that no live Runner owns — every run left behind by a previous
// daemon, plus any whose owner has already gone away.
//
// One per runs directory, cached by the caller: the construction cost is per
// gaggle, not per run.
func (s StallServices) newStalledTerminalizer(
	runLayout instance.Layout,
	runsDir string,
	log *journal.InstanceLog,
	deps *StalledSweepDeps,
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
	manager, err := worktree.NewManager(runLayout.WorkcopiesDir(), s.CleanupGuard(runsDir))
	if err != nil {
		return nil, fmt.Errorf("construct stalled-run worktree manager for %s: %w", runsDir, err)
	}
	terminalizer, err := runner.New(runner.Config{
		Worktrees:       manager,
		RunsDir:         runsDir,
		PrepareTerminal: terminalPreparer,
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return s.FinalizeTerminal(runLayout, log, manager, runID)
		},
		NotifyTerminal: notify,
		// Without this the run.finished this terminalizer appends is invisible
		// to every derived reader — see StalledSweepDeps.JournalAdvanced.
		JournalAdvanced:        deps.journalAdvanced(),
		JournalAdvancedContext: deps.journalAdvancedContext(),
	})
	if err != nil {
		return nil, fmt.Errorf("construct stalled-run terminalizer for %s: %w", runsDir, err)
	}
	return terminalizer, nil
}

// Sweep terminalizes runs past their pinned inactivity or duration limits.
//
// SweepStalledRuns terminalizes runs whose journals have gone quiet past
// their pinned stalledRunTimeout (or past maxRunDuration).
//
// A legacy engine-driven run is left untouched on disk because goobers no
// longer owns its workflow. Its reconstructed local concurrency slot is
// released so it cannot block local scheduling after an upgrade.
func (s StallServices) Sweep(
	ctx context.Context,
	l instance.Layout,
	runners RunnerResolver,
	fallback *runner.Runner,
	log *journal.InstanceLog,
	deps *StalledSweepDeps,
	notify runner.TerminalNotifier,
	release func(runID, workflow string),
	now time.Time,
	timeout time.Duration,
	maxDuration time.Duration,
	recoveryRunDirs ...[]string,
) error {
	candidates, err := s.Candidates(ctx, l, recoveryRunDirs...)
	if err != nil {
		return err
	}

	// Accumulate per-entry failures into a slice, then bound the aggregate at
	// the end: a sweep over a pathological number of orphan/bad run directories
	// must never build an unbounded error message that then bloats the
	// scheduler journal when persisted (#1166, #1414).
	var sweepErrs []error
	terminalizers := make(map[string]*runner.Runner)
	suspended := deps.hostSuspensions()
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
		events, elapsed, elapsedErr := s.Clock.Read(reader, identity.StartedAt, now, suspended)
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
			if !lastActivity.Before(now.Add(-stallWindow - hostsuspend.Overlap(suspended, lastActivity, now))) {
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
		//
		// A cancellation that no worker ever picks up is escalated to a
		// server-side terminate one stall timeout after it was first
		// requested; see settleStalledEngineRun.
		if identity.EngineDriven() {
			if release != nil {
				release(identity.RunID, identity.Workflow)
			}
			continue
		}

		runLayout := l
		if filepath.Clean(runsDir) != filepath.Clean(l.RunsDir()) {
			rootGaggle := filepath.Base(filepath.Dir(runsDir))
			runLayout = l.ForGaggle(rootGaggle)
		}
		runRunner, liveOwner := fallback, false
		if runners != nil {
			runRunner, liveOwner = runners.Resolve(identity.RunID, runLayout.Gaggle(), fallback)
		}
		if runRunner == nil {
			runRunner = terminalizers[runsDir]
			if runRunner == nil {
				runRunner, err = s.newStalledTerminalizer(runLayout, runsDir, log, deps, notify)
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
			result, terminated, err = runRunner.ExpireRun(identity.RunID, now, identity.StartedAt, runMaxDuration, suspended...)
		} else {
			result, terminated, err = runRunner.EscalateStalled(identity.RunID, now, stallWindow, suspended...)
		}
		if terminated {
			if release != nil {
				release(identity.RunID, identity.Workflow)
			}
			if log != nil && !liveOwner {
				terminalPhase := journal.PhaseEscalated
				errorCode := runsemantics.RunStalledErrorCode
				message := fmt.Sprintf("run exceeded %s without journal activity", runTimeout)
				if durationExceeded {
					terminalPhase = journal.PhaseAborted
					errorCode = runsemantics.RunDurationExceededErrorCode
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
func (JournalActivityClock) Read(reader *journal.Reader, startedAt, now time.Time, suspended []hostsuspend.Window) ([]journal.Event, time.Duration, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, 0, err
	}
	elapsed, err := runner.RunExecutionElapsed(events, startedAt, now, suspended...)
	return events, elapsed, err
}

// RunnerResolver leaves live runner ownership in the daemon composition root.
type RunnerResolver interface {
	Resolve(string, string, *runner.Runner) (*runner.Runner, bool)
}

// ActivityClock supplies both the journal activity and active execution time.
type ActivityClock interface {
	Read(*journal.Reader, time.Time, time.Time, []hostsuspend.Window) ([]journal.Event, time.Duration, error)
}

// JournalActivityClock reads activity and active execution time from the run journal.
type JournalActivityClock struct{}

// DowntimeSource reports only graceful-drain time after the last activity.
type DowntimeSource interface{ Since(time.Time) time.Duration }

// SuspensionSource supplies observed host suspension windows.
type SuspensionSource interface{ Windows() []hostsuspend.Window }

// StallServices are the command-owned operations used by a sweep.
type StallServices struct {
	Candidates       func(context.Context, instance.Layout, ...[]string) ([]string, error)
	CleanupGuard     func(string) worktree.ManagerOption
	FinalizeTerminal func(instance.Layout, *journal.InstanceLog, *worktree.Manager, string) error
	Clock            ActivityClock
}
