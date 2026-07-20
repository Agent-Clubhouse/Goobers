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
)

func sweepStalledRuns(
	l instance.Layout,
	runners map[string]*runner.Runner,
	fallback *runner.Runner,
	log *journal.InstanceLog,
	release func(runID, workflow string),
	now time.Time,
	timeout time.Duration,
) error {
	runDirs, err := l.RunDirs()
	if err != nil {
		return err
	}

	var sweepErr error
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
			if filepath.Clean(runsDir) != filepath.Clean(l.RunsDir()) {
				runRunner = runners[identity.Gaggle]
			}
			if runRunner == nil {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("run %q has no runner for gaggle %q", identity.RunID, identity.Gaggle))
				continue
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
