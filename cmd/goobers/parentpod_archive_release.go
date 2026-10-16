package main

import (
	"context"
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

func releaseTerminalParentArchives(layout instance.Layout, manager *worktree.Manager, runID string) error {
	reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), runID))
	if errors.Is(err, os.ErrNotExist) {
		return nil // Legacy finalization without a journal has no parent receipts.
	}
	if err != nil {
		return err
	}
	work, err := readParentRetirementWork(reader)
	if err != nil || work.empty() {
		return err
	}

	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	cloneURL := repoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	restorer := parentArchiveRestorer{layout: layout, config: cfg, worktrees: manager, cloneURL: cloneURL}
	if service, ok := stageGrantMinterFor(layout.Root).(*daemonCredentialService); ok {
		restorer.scrubber = service.shared
	}
	if err := restorer.retryRetirement(reader); err != nil {
		return fmt.Errorf("%w: retry parent retirement: %w", worktree.ErrCleanupDeferred, err)
	}
	candidates, err := runner.ParentRetirementCandidates(reader)
	if err != nil {
		return err
	}
	return restorer.releaseArchives(reader, candidates)
}

func (r parentArchiveRestorer) releaseArchives(reader *journal.Reader, candidates []runner.ParentRetirementCandidate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var failures error
	for _, candidate := range candidates {
		if candidate.RetirementSeq == 0 {
			failures = errors.Join(failures, errors.New("parent workspace still requires durable retirement"))
			continue
		}
		failures = errors.Join(failures, r.releaseArchive(ctx, reader, candidate.Workspace))
	}
	if failures != nil {
		return fmt.Errorf("%w: release archived parent workspace: %w", worktree.ErrCleanupDeferred, failures)
	}
	return nil
}

func (r parentArchiveRestorer) releaseArchive(ctx context.Context, reader *journal.Reader, archive runner.ParentWorkspaceArchive) error {
	record, _, err := r.authorize(ctx, reader, archive)
	if err != nil {
		return err
	}
	project, err := recoveryConfiguredProject(r.config, record.RepositoryKey)
	if err != nil {
		return err
	}
	url, err := r.cloneURL(project)
	if err != nil {
		return err
	}
	return r.worktrees.ReleaseArchivedStage(ctx, url, archive.Custody.Workspace, func(ctx context.Context, target worktree.CleanupTarget) error {
		found, err := retiredParentCleanup(ctx, r.layout, r.config, r.worktrees, reader, record.RepositoryKey, target)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("parent archive no longer authorizes workspace release")
		}
		return nil
	})
}
