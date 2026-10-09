package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// Called only by terminal retirement while it owns the journal. Never delete
// source pins from a concurrent hold-release sweep: resume may be importing them.
func (r parentArchiveRestorer) releaseForkSources(ctx context.Context, run *journal.Run, reader *journal.Reader) error {
	values, err := runner.RetiredParentForks(reader)
	if err != nil {
		return err
	}
	for _, value := range values {
		if err := r.releaseForkSource(ctx, reader, value); err != nil {
			return err
		}
		if err := runner.RecordParentForkSourceRelease(run, value); err != nil {
			return err
		}
	}
	return nil
}

func (r parentArchiveRestorer) releaseForkSource(ctx context.Context, reader *journal.Reader, value runner.ParentForkRetirement) error {
	source, err := parallelworkspace.ReadSource(reader, value.Plan.Source, value.Plan.Parallel, value.Plan.Sequence)
	if err != nil {
		return err
	}
	project, err := recoveryConfiguredProject(r.config, source.Record.RepositoryKey)
	if err != nil {
		return err
	}
	url, err := r.cloneURL(project)
	if err != nil {
		return err
	}
	for _, owner := range value.Plan.Workspaces {
		if owner.RepositoryDigest != worktree.RepositoryDigest(url) {
			return errors.New("parallel source release repository changed")
		}
	}
	found, err := r.worktrees.WithExistingMirror(ctx, url, func(repository string) error {
		if err := r.verifyForkArchives(ctx, reader, repository, value); err != nil {
			return err
		}
		if err := r.releaseForkResultPins(ctx, reader, repository, value); err != nil {
			return err
		}
		retained, err := r.forkSourceRetained(ctx, source.Record)
		if err != nil || retained {
			return err
		}
		return recovery.DeleteSnapshotRef(ctx, repository, source.Record)
	})
	if err == nil && !found {
		return errors.New("parallel source release mirror unavailable")
	}
	return err
}

func (r parentArchiveRestorer) verifyForkArchives(ctx context.Context, reader *journal.Reader, repository string, value runner.ParentForkRetirement) error {
	candidates, err := runner.ParentRetirementCandidates(reader)
	if err != nil {
		return err
	}
	policy, _ := resolveRecoveryPolicy(r.layout, r.config)
	for _, owner := range value.Plan.Workspaces {
		found := false
		for _, candidate := range candidates {
			if candidate.Workspace.Custody.Workspace != owner {
				continue
			}
			found = true
			record, authority, err := r.authorize(ctx, reader, candidate.Workspace)
			if err != nil {
				return err
			}
			state, err := recovery.LoadRetainedParentState(ctx, repository, filepath.Join(r.layout.Root, "recovery"), recoveryOverflowRoot(r.layout), record, policy.MaxArchiveBytesEffective())
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(state.Policy, authority.policy) {
				return errors.New("parallel source release archive policy changed")
			}
		}
		if !found {
			return errors.New("parallel source release archive unavailable")
		}
	}
	return nil
}

// A recovery ref may also belong to an older retained archive. Its existing
// retention owner then controls expiry; temporary fork ownership must not unpin it.
func (r parentArchiveRestorer) forkSourceRetained(ctx context.Context, source recovery.Record) (bool, error) {
	entries, err := recovery.ReadInventory(ctx, filepath.Join(r.layout.Root, "recovery"), recovery.MaxInventoryEntries)
	if err != nil {
		return false, err
	}
	overflow, unreadable, err := recovery.ReadOverflow(ctx, recoveryOverflowRoot(r.layout))
	if err != nil {
		return false, err
	}
	if len(unreadable) != 0 {
		return false, errors.New("parallel source release cannot verify overflow custody")
	}
	for _, entry := range append(entries, overflow...) {
		record := entry.Record
		if record.RepositoryKey == source.RepositoryKey && record.Ref == source.Ref {
			if record.SnapshotSHA != source.SnapshotSHA {
				return false, recovery.ErrRecordConflict
			}
			return true, nil
		}
	}
	return false, nil
}

// The archive can be a delta against the synthetic source commit. Reimport its
// durable carrier before loading the archive; no live root is recaptured.
func (r parentArchiveRestorer) importArchivedForkSource(ctx context.Context, rec runner.OwnedJournalRecorder, reader *journal.Reader, archive runner.ParentWorkspaceArchive) error {
	plan, found, err := runner.ParentForkPlanForWorkspace(reader, archive.Custody.Workspace)
	if err != nil || !found {
		return err
	}
	source, err := parallelworkspace.ReadSource(reader, plan.Source, plan.Parallel, plan.Sequence)
	if err != nil {
		return err
	}
	project, err := recoveryConfiguredProject(r.config, source.Record.RepositoryKey)
	if err != nil {
		return err
	}
	backend := parallelworkspace.Service{Worktrees: r.worktrees, CloneURL: r.cloneURL}
	_, err = backend.Prepare(ctx, rec, spec.Request{RunID: plan.RunID, Gaggle: plan.Gaggle, Parallel: plan.Parallel, Sequence: plan.Sequence, At: source.Record.CreatedAt, Repository: project}, &plan.Source)
	return err
}

func (r parentArchiveRestorer) releaseForkResultPins(ctx context.Context, reader *journal.Reader, repository string, value runner.ParentForkRetirement) error {
	results, err := runner.ParentForkResults(reader, value.Plan, value.Reference)
	if err != nil {
		return err
	}
	for _, result := range results {
		request := spec.ResultRequest{Request: spec.Request{Parallel: value.Plan.Parallel, Sequence: value.Plan.Sequence}, Plan: value.Reference, Seed: value.Plan.Source, Branch: result.Branch, Status: result.Status, Custody: value.Plan.Workspaces[result.Branch-1]}
		snapshot, err := parallelworkspace.ReadResult(reader, request, result.Source)
		if err != nil {
			return err
		}
		retained, err := r.forkSourceRetained(ctx, snapshot.Record)
		if err != nil {
			return err
		}
		if !retained {
			if err := recovery.DeleteSnapshotRef(ctx, repository, snapshot.Record); err != nil {
				return err
			}
		}
	}
	return nil
}
