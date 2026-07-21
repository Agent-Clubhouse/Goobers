package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type stalledTerminalPreparer func(instance.Layout) (runner.TerminalPreparer, error)

// daemonRunnerRegistry retains each live run's owning Runner while atomically
// swapping the configured fallback runners during config reload.
type daemonRunnerRegistry struct {
	mu      sync.RWMutex
	current map[string]*runner.Runner
	owners  map[string]*runner.Runner
}

func newDaemonRunnerRegistry() *daemonRunnerRegistry {
	return &daemonRunnerRegistry{owners: make(map[string]*runner.Runner)}
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

func (r *daemonRunnerRegistry) Track(runID string, owner *runner.Runner) func() {
	if r == nil || owner == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.owners == nil {
		r.owners = make(map[string]*runner.Runner)
	}
	if _, exists := r.owners[runID]; exists {
		r.mu.Unlock()
		return func() {}
	}
	r.owners[runID] = owner
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.owners[runID] == owner {
			delete(r.owners, runID)
		}
		r.mu.Unlock()
	}
}

func (r *daemonRunnerRegistry) Resolve(runID, gaggle string, fallback *runner.Runner) (*runner.Runner, bool) {
	if r == nil {
		return fallback, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if owner := r.owners[runID]; owner != nil {
		return owner, true
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
) error {
	runDirs, err := l.RunDirs()
	if err != nil {
		return err
	}

	var sweepErr error
	terminalizers := make(map[string]*runner.Runner)
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("read runs directory %s: %w", runsDir, err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runDir := filepath.Join(runsDir, entry.Name())
			reader, err := journal.OpenRead(runDir)
			if err != nil {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("inspect run directory %q: %w", entry.Name(), err))
				continue
			}
			identity, err := reader.Identity()
			if err != nil {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("read run %q identity: %w", entry.Name(), err))
				continue
			}
			phase, err := reader.Phase()
			if err != nil {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("read run %q phase: %w", identity.RunID, err))
				continue
			}
			if phase != journal.PhaseRunning {
				continue
			}
			events, err := reader.Events()
			if err != nil {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("read run %q events: %w", identity.RunID, err))
				continue
			}
			if len(events) == 0 {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("running run %q has no journal events", identity.RunID))
				continue
			}
			if events[len(events)-1].Type == journal.EventGatePaused {
				continue
			}
			if !events[len(events)-1].Time.Before(now.Add(-timeout)) {
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
					var terminalPreparer runner.TerminalPreparer
					if prepare != nil {
						terminalPreparer, err = prepare(runLayout)
						if err != nil {
							sweepErr = errors.Join(sweepErr, fmt.Errorf("construct stalled-run terminal preparer for %s: %w", runsDir, err))
							continue
						}
					}
					manager, managerErr := worktree.NewManager(runLayout.WorkcopiesDir())
					if managerErr != nil {
						sweepErr = errors.Join(sweepErr, fmt.Errorf("construct stalled-run worktree manager for %s: %w", runsDir, managerErr))
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
						sweepErr = errors.Join(sweepErr, fmt.Errorf("construct stalled-run terminalizer for %s: %w", runsDir, err))
						continue
					}
					terminalizers[runsDir] = runRunner
				}
			}
			result, escalated, err := runRunner.EscalateStalled(identity.RunID, now, timeout)
			if escalated {
				if release != nil {
					release(identity.RunID, identity.Workflow)
				}
				if log != nil && !liveOwner {
					appendErr := log.Append(journal.Event{
						Type:     journal.EventRunFinished,
						Gaggle:   identity.Gaggle,
						Workflow: identity.Workflow,
						RunID:    identity.RunID,
						Status:   string(journal.PhaseEscalated),
						Error: &journal.ErrorDetail{
							Code:    runner.RunStalledErrorCode,
							Message: fmt.Sprintf("run exceeded %s without journal activity", timeout),
						},
					})
					err = errors.Join(err, appendErr)
				}
			}
			if err != nil {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("escalate stalled run %q (%s): %w", identity.RunID, result.Phase, err))
			}
		}
	}
	return sweepErr
}
