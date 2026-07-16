package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// finalizeTerminalRun performs every instance-level terminal cleanup action.
// It is idempotent because both worktree finalization and claim release are.
func finalizeTerminalRun(l instance.Layout, log *journal.InstanceLog, wtMgr *worktree.Manager, runID string) error {
	results, worktreeErr := wtMgr.FinalizeRun(context.Background(), runID)

	var annotationErr error
	annotationLog := log
	closeAnnotationLog := false
	for _, result := range results {
		if !result.Kept {
			continue
		}
		journaled, err := keptWorktreeJournaled(l.SchedulerDir(), runID, result.WorktreeID)
		if err != nil {
			annotationErr = errors.Join(annotationErr, fmt.Errorf("inspect kept worktree annotation %s: %w", result.WorktreeID, err))
			continue
		}
		if journaled {
			continue
		}
		event := journal.Event{
			Type: journal.EventRunnerAnnotation,
			Runner: map[string]any{
				"worktreeID":     result.WorktreeID,
				"worktreeStatus": "kept",
			},
		}
		event.RunID = runID
		if annotationLog == nil {
			annotationLog, _, err = journal.OpenInstanceLog(l.SchedulerDir())
			if err != nil {
				annotationErr = errors.Join(annotationErr, fmt.Errorf("open instance journal for kept worktree %s: %w", result.WorktreeID, err))
				continue
			}
			closeAnnotationLog = true
		}
		if err := annotationLog.Append(event); err != nil {
			annotationErr = errors.Join(annotationErr, fmt.Errorf("journal kept worktree %s: %w", result.WorktreeID, err))
		}
	}
	if closeAnnotationLog {
		annotationErr = errors.Join(annotationErr, annotationLog.Close())
	}

	claimErr := releaseClaimsForRun(l, log, runID)
	return errors.Join(worktreeErr, annotationErr, claimErr)
}

func keptWorktreeJournaled(schedulerDir, runID, worktreeID string) (bool, error) {
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.RunID == runID &&
			event.Runner["worktreeID"] == worktreeID &&
			event.Runner["worktreeStatus"] == "kept" {
			return true, nil
		}
	}
	return false, nil
}
