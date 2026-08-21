package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// finalizeTerminalRun performs every instance-level terminal cleanup action.
// It is idempotent because both worktree finalization and claim release are.
func finalizeTerminalRun(l instance.Layout, log *journal.InstanceLog, wtMgr *worktree.Manager, runID string) error {
	return finalizeTerminalRunWithClaimRelease(l, log, wtMgr, runID, releaseClaimsForRun)
}

func finalizeTerminalRunForRecovery(l instance.Layout, log *journal.InstanceLog, wtMgr *worktree.Manager, runID string) error {
	return finalizeTerminalRunWithClaimRelease(l, log, wtMgr, runID, releaseClaimsForRunWithDefaultTimeout)
}

// finalizeTerminalRunWithClaimMarkers is finalizeTerminalRun plus the
// provider-side claim-epoch release (#3347): the goobers:claimed mirror is
// retired in the same terminal cleanup step that releases the ledger lease, so
// claims.json and the provider cannot disagree for a full backlog-curation
// interval after a run that terminates without reaching issue-close-out — the
// `no-work` outcome being the case that makes that a certainty rather than an
// edge case. A nil release (repo-less instance, non-GitHub provider) is exactly
// finalizeTerminalRun.
func finalizeTerminalRunWithClaimMarkers(
	l instance.Layout,
	log *journal.InstanceLog,
	wtMgr *worktree.Manager,
	runID string,
	repo providers.RepositoryRef,
	releaseMarker claimMarkerReleaseFunc,
) error {
	return finalizeTerminalRunWithClaimRelease(l, log, wtMgr, runID,
		func(l instance.Layout, log *journal.InstanceLog, runID string) error {
			// Provider marker first, ledger second — the order
			// issue-close-out and backlog-query --release already use, and the
			// order that keeps this run's ownership of the epoch it is closing
			// provable (releaseTerminalClaimMarkers' doc).
			releaseTerminalClaimMarkers(l, log, runID, repo, releaseMarker)
			return releaseClaimsForRun(l, log, runID)
		})
}

func finalizeTerminalRunWithClaimRelease(l instance.Layout, log *journal.InstanceLog, wtMgr *worktree.Manager, runID string, release func(instance.Layout, *journal.InstanceLog, string) error) error {
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

	noOpErr := recordPRRemediationNoop(l, runID)
	var claimErr error
	if noOpErr == nil {
		claimErr = release(l, log, runID)
	} else if isJournaledClaimsLockTimeout(noOpErr) {
		noOpErr = nil
	}
	if isJournaledClaimsLockTimeout(claimErr) {
		claimErr = nil
	}
	return errors.Join(worktreeErr, annotationErr, noOpErr, claimErr)
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
