package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type parentArchiveRestorer struct {
	layout    instance.Layout
	config    *instance.Config
	worktrees *worktree.Manager
	cloneURL  func(apiv1.RepoRef) (string, error)
	scrubber  journal.Scrubber
}

func (r parentArchiveRestorer) restore(ctx context.Context, rec runner.OwnedJournalRecorder, archive runner.ParentWorkspaceArchive, seq uint64) error {
	pending, err := runner.ParentArchiveRestorationPending(rec, archive, seq)
	if err != nil || !pending {
		return err
	}
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	record, authority, err := r.authorize(ctx, reader, archive)
	if err != nil {
		return err
	}
	id := authority.identity
	_, branch, err := runner.OwnedJournalScope(rec)
	if err != nil {
		return err
	}
	if authority.branch != branch {
		return errors.New("parent restore contract has different workspace scope")
	}
	project, err := recoveryConfiguredProject(r.config, record.RepositoryKey)
	if err != nil {
		return err
	}
	url, err := r.cloneURL(project)
	if err != nil {
		return err
	}
	if archive.Custody.Workspace.RepositoryDigest != worktree.RepositoryDigest(url) {
		return errors.New("parent restore repository differs from custody")
	}
	if err := r.importArchivedForkSource(ctx, rec, reader, archive); err != nil {
		return err
	}
	policy, _ := resolveRecoveryPolicy(r.layout, r.config)
	maxBytes := policy.MaxArchiveBytesEffective()
	var state recovery.RetainedParentState
	if err := r.worktrees.WithRecoveryMirror(ctx, url, func(repository string) error {
		var err error
		state, err = recovery.LoadRetainedParentState(ctx, repository, filepath.Join(r.layout.Root, "recovery"), recoveryOverflowRoot(r.layout), record, maxBytes)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state.Policy, authority.policy) {
			return errors.New("parent archive exclusion policy changed")
		}
		return nil
	}); err != nil {
		return err
	}
	custody := archive.Custody.Workspace
	wt, err := r.worktrees.CreateParentRestore(ctx, worktree.ParentRestoreOptions{RepoURL: url, RunID: custody.WorkspaceID, OwnerRunID: id.RunID, Gaggle: id.Gaggle, Branch: custody.Branch, BaseRef: record.BaseSHA, HeadSHA: state.HeadSHA, StartRef: custody.StartRef})
	if err != nil {
		return err
	}
	held, err := wt.HoldForChild(ctx)
	if err != nil {
		return err
	}
	if held != custody {
		return errors.New("restored parent checkout custody changed")
	}
	if err := applyParentArchiveRestore(ctx, rec, reader, wt.Path, archive, seq, record, maxBytes); err != nil {
		return err
	}
	return runner.RecordParentArchiveRestoration(rec, archive, seq)
}

func (r parentArchiveRestorer) authorize(ctx context.Context, reader *journal.Reader, archive runner.ParentWorkspaceArchive) (recovery.Record, parentArchiveAuthority, error) {
	var record recovery.Record
	var empty parentArchiveAuthority
	id, err := reader.Identity()
	if err != nil {
		return record, empty, err
	}
	if id.Child != nil || id.RunID != archive.Custody.Workspace.OwnerRunID || r.layout.RunsDir() != filepath.Dir(reader.Dir()) {
		return record, empty, errors.New("parent archive restore owner mismatch")
	}
	if archive.Archive.Integrity != apiv1.IntegrityTrusted {
		return record, empty, errors.New("parent archive metadata lacks host provenance")
	}
	data, err := reader.ArtifactBytesBounded(archive.Archive, 16384)
	if err != nil {
		return record, empty, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, empty, err
	}
	if err := record.ValidateRestorable(); err != nil {
		return record, empty, err
	}
	if record.RunID != id.RunID {
		return record, empty, errors.New("parent archive record belongs to another run")
	}
	authority, err := parentArchiveSource(ctx, reader, archive)
	if err != nil {
		return record, empty, err
	}
	if authority.repositoryKey != record.RepositoryKey {
		return record, empty, errors.New("parent archive repository changed")
	}
	return record, authority, nil
}
