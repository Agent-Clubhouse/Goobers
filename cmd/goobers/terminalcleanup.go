package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
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
	if err := installTerminalRecoveryGuard(l, wtMgr); err != nil {
		return err
	}
	results, worktreeErr := wtMgr.FinalizeRun(context.Background(), runID)
	if retainErr := retainTerminalRunBranch(l, wtMgr, runID); retainErr != nil {
		worktreeErr = errors.Join(worktreeErr, fmt.Errorf("%w: retain terminal branch for %s: %w", worktree.ErrCleanupDeferred, runID, retainErr))
	}
	if renewErr := renewTerminalRecovery(l, runID); renewErr != nil {
		worktreeErr = errors.Join(worktreeErr, fmt.Errorf("%w: renew terminal recovery for %s: %w", worktree.ErrCleanupDeferred, runID, renewErr))
	}

	var annotationErr error
	annotationLog := log
	closeAnnotationLog := false
	for _, result := range results {
		worktreeStatus := ""
		if result.Kept {
			worktreeStatus = "kept"
		} else if result.CleanupDisposition != "" {
			worktreeStatus = "cleanup-retained"
		}
		if worktreeStatus == "" {
			continue
		}
		journaled, err := worktreeDispositionJournaled(l.SchedulerDir(), runID, result.WorktreeID, worktreeStatus)
		if err != nil {
			annotationErr = errors.Join(annotationErr, fmt.Errorf("inspect worktree disposition annotation %s: %w", result.WorktreeID, err))
			continue
		}
		if journaled {
			continue
		}
		event := journal.Event{
			Type: journal.EventRunnerAnnotation,
			Runner: map[string]any{
				"worktreeID":     result.WorktreeID,
				"worktreeStatus": worktreeStatus,
			},
		}
		if result.CleanupDisposition != "" {
			event.Runner["cleanupDisposition"] = result.CleanupDisposition
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
	err := errors.Join(worktreeErr, annotationErr, noOpErr, claimErr)
	if err == nil {
		err = journal.ClearRunActive(filepath.Join(l.RunsDir(), runID))
	}
	return err
}

func retainTerminalRunBranch(l instance.Layout, manager *worktree.Manager, runID string) error {
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	var branch string
	for _, event := range events {
		if event.Type == journal.EventRefTouched && event.ExternalRef != nil && event.ExternalRef.Kind == "branch" {
			branch = strings.TrimSpace(event.ExternalRef.ID)
		}
	}
	if branch == "" {
		return nil
	}
	pinned := false
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["workspaceMode"] == "pinned" {
			pinned = true
			break
		}
	}
	if exists, inventoryErr := terminalRecoveryExists(l, runID); inventoryErr != nil {
		return inventoryErr
	} else if exists {
		return nil
	}
	project := apiv1.RepoRef{}
	if identity.WorkspaceRepository != nil {
		project = *identity.WorkspaceRepository
	} else {
		project, err = terminalGaggleProject(l)
		if err != nil {
			return err
		}
	}
	if project.Name == "" || strings.TrimSpace(project.Branch) == "" {
		return fmt.Errorf("terminal recovery requires the run repository and base branch")
	}
	cloneURL := repoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	url, err := cloneURL(project)
	if err != nil {
		return err
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return err
	}
	identities, err := recoveryRepositoryIdentities(cfg, cloneURL)
	if err != nil {
		return err
	}
	target := func(path string, pinned bool) worktree.CleanupTarget {
		worktreeID := "terminal-branch"
		baseRef := project.Branch
		if pinned {
			worktreeID = "terminal-pinned"
			baseRef = "refs/remotes/mirror/" + project.Branch
		}
		return worktree.CleanupTarget{
			Path: path, WorktreeID: worktreeID, OwnerRunID: runID,
			Gaggle: identity.Gaggle, BaseRef: baseRef, Pinned: pinned,
			RepositoryDigest: worktree.RepositoryDigest(url), CreatedAt: identity.StartedAt,
		}
	}
	callback := recoveryCleanupHandler(l, cfg, manager.Root, identities, journal.NewRegistryScrubber(), true, manager, nil)
	if pinned {
		found, err := manager.WithPinnedWorkspaceOwnedBy(context.Background(), url, runID, branch, func(path string) error {
			return callback(context.Background(), target(path, true))
		})
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("terminal recovery pinned workspace for branch %q is not held by run %s", branch, runID)
		}
		return nil
	}
	found, err := manager.WithExistingBranchCheckout(context.Background(), url, branch, project.Branch, func(path string) error {
		return callback(context.Background(), target(path, false))
	})
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	return fmt.Errorf("terminal recovery branch %q is not present in the managed mirror", branch)
}

func terminalRecoveryExists(l instance.Layout, runID string) (bool, error) {
	cfg, _ := resolveRecoveryPolicy(l, nil)
	entries, err := recovery.ReadInventory(context.Background(), filepath.Join(l.Root, "recovery"), cfg.MaxSnapshotsEffective())
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Record.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

func worktreeDispositionJournaled(schedulerDir, runID, worktreeID, status string) (bool, error) {
	recorded, err := annotationsForInstance(schedulerDir).worktreeState(schedulerDir, runID, worktreeID)
	return recorded == status, err
}
