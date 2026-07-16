package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// finalizeTerminalRun performs every instance-level terminal cleanup action.
// It is idempotent because both worktree finalization and claim release are.
func finalizeTerminalRun(l instance.Layout, log *journal.InstanceLog, wtMgr *worktree.Manager, jr *journal.Run, runID string) error {
	results, worktreeErr := wtMgr.FinalizeRun(context.Background(), runID)

	var annotationErr error
	for _, result := range results {
		if !result.Kept {
			continue
		}
		journaled, err := keptWorktreeJournaled(l.RunsDir(), runID, result.WorktreeID)
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
		switch {
		case jr != nil:
			if err := jr.Append(event); err != nil {
				annotationErr = errors.Join(annotationErr, fmt.Errorf("journal kept worktree %s: %w", result.WorktreeID, err))
			}
		case log != nil:
			event.RunID = runID
			if err := log.Append(event); err != nil {
				annotationErr = errors.Join(annotationErr, fmt.Errorf("journal kept worktree %s: %w", result.WorktreeID, err))
			}
		}
	}

	claimErr := releaseClaimsForRun(l, log, runID)
	return errors.Join(worktreeErr, annotationErr, claimErr)
}

func keptWorktreeJournaled(runsDir, runID, worktreeID string) (bool, error) {
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return false, err
	}
	events, err := rd.Events()
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["worktreeID"] == worktreeID &&
			event.Runner["worktreeStatus"] == "kept" {
			return true, nil
		}
	}
	return false, nil
}
