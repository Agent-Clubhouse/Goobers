package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type stalledTerminalPreparer func(instance.Layout) (runner.TerminalPreparer, error)

func sweepStalledRuns(
	l instance.Layout,
	runners map[string]*runner.Runner,
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
			if !events[len(events)-1].Time.Before(now.Add(-timeout)) {
				continue
			}

			runRunner := fallback
			runLayout := l
			if filepath.Clean(runsDir) != filepath.Clean(l.RunsDir()) {
				rootGaggle := filepath.Base(filepath.Dir(runsDir))
				runLayout = l.ForGaggle(rootGaggle)
				runRunner = runners[rootGaggle]
			}
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
				if log != nil {
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
