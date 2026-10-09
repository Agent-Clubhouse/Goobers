package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// Retirement is an owned post-terminal operation, separate from releasing a
// hold. An archive failure leaves the original checkout protected for retry.
func (r parentArchiveRestorer) retire(run *journal.Run) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	pending, err := runner.PendingParentForks(reader)
	if err != nil {
		return err
	}
	candidates, err := retirementCandidatesBeforeForkRecovery(reader, pending)
	if err != nil || len(candidates) == 0 && len(pending) == 0 {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if filepath.Dir(reader.Dir()) != r.layout.RunsDir() {
		return errors.New("parent retirement journal belongs to another layout")
	}
	captureAt, terminal, err := recoveryCaptureWindow(ctx, reader, id.StartedAt)
	if err != nil {
		return err
	}
	if !terminal {
		return errors.New("parent retirement requires a durable terminal event")
	}
	if err := verifyParentArchiveChildren(ctx, r.layout, id); err != nil {
		return err
	}
	if err := r.recoverForkPlans(ctx, run, reader, pending); err != nil {
		return err
	}
	candidates, err = runner.ParentRetirementCandidates(reader)
	if err != nil {
		return err
	}
	var failures error
	for _, candidate := range candidates {
		if candidate.RetirementSeq != 0 {
			continue
		}
		recorder, err := runner.OwnedBranchRecorder(run, candidate.Branch)
		if err == nil {
			err = r.captureRetirement(ctx, reader, recorder, candidate, captureAt)
		}
		failures = errors.Join(failures, err)
	}
	return failures
}

func (r parentArchiveRestorer) captureRetirement(ctx context.Context, reader *journal.Reader, rec runner.OwnedJournalRecorder, candidate runner.ParentRetirementCandidate, captureAt time.Time) error {
	wt, key, url, err := r.retirementWorkspace(ctx, reader, candidate)
	if err != nil {
		return err
	}
	target, err := wt.HeldCleanupTarget(ctx)
	if err != nil {
		return err
	}
	found, err := r.worktrees.WithExistingMirror(ctx, url, func(string) error {
		return r.captureHeldRetirement(ctx, reader, rec, candidate, captureAt, wt, key, target)
	})
	if err == nil && !found {
		err = errors.New("parent retirement mirror unavailable")
	}
	return err
}

// recoveryCleanupRequest's capacity eviction callback requires this managed
// repository lock, just like an ordinary cleanup guard.
func (r parentArchiveRestorer) captureHeldRetirement(ctx context.Context, reader *journal.Reader, rec runner.OwnedJournalRecorder, candidate runner.ParentRetirementCandidate, captureAt time.Time, wt *worktree.Worktree, key string, target worktree.CleanupTarget) error {
	policy, err := parentCleanupPolicy(ctx, reader, target, key)
	if err != nil {
		return err
	}
	if policy == nil {
		return errors.New("parent retirement requires verified source policy")
	}
	scrubber := r.scrubber
	if scrubber == nil {
		scrubber = journal.NewRegistryScrubber()
	}
	publication := recoveryCleanupJournal{directory: r.layout.SchedulerDir(), scrubber: scrubber}
	request, err := recoveryCleanupRequest(r.layout, r.config, r.worktrees.Root, r.worktrees, key, wt.Path, target.OwnerRunID, captureAt, publication)
	if err != nil {
		return err
	}
	request.BaseRef, request.ParentPolicy = target.BaseRef, policy
	record, _, err := recovery.Retain(ctx, request, publication)
	if err != nil {
		return err
	}
	// Retain acknowledged either a verified bundle or the existing durable
	// mirror pin tier. Cleanup revalidates that exact retention source.
	if err := record.ValidateRestorable(); err != nil {
		return err
	}
	if err := recovery.VerifyRetainedParentCheckout(ctx, wt.Path, record, request.MaxArchiveBytes); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ref, err := rec.RecordArtifactBoundedWithIntegrity("parent-archive-record.json", data, apiv1.IntegrityTrusted, 16384)
	if err != nil {
		return err
	}
	if ref.Digest != journal.Digest(data) {
		return errors.New("parent archive metadata changed during recording")
	}
	return runner.RecordParentArchiveRetirement(rec, candidate.Workspace.Custody.Workspace.Branch, ref)
}

func (r parentArchiveRestorer) retirementWorkspace(ctx context.Context, reader *journal.Reader, candidate runner.ParentRetirementCandidate) (*worktree.Worktree, string, string, error) {
	value := candidate.Workspace
	authority, err := parentArchiveSource(ctx, reader, value)
	if err != nil {
		return nil, "", "", err
	}
	if authority.branch != candidate.Branch {
		return nil, "", "", errors.New("parent retirement branch changed")
	}
	key := authority.repositoryKey
	project, err := recoveryConfiguredProject(r.config, key)
	if err != nil {
		return nil, "", "", err
	}
	url, err := r.cloneURL(project)
	if err != nil {
		return nil, "", "", err
	}
	wt, err := r.worktrees.AdoptHeldStage(ctx, url, value.Custody.Workspace)
	return wt, key, url, err
}

func retirementCandidatesBeforeForkRecovery(reader *journal.Reader, pending []runner.ParentForkRecovery) ([]runner.ParentRetirementCandidate, error) {
	candidates, err := runner.ParentRetirementCandidates(reader)
	if errors.Is(err, runner.ErrParentReturnPending) && len(pending) != 0 {
		return nil, nil
	}
	return candidates, err
}
