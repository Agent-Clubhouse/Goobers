package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
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
	record, contract, err := r.authorize(ctx, reader, archive)
	if err != nil {
		return err
	}
	id := contract.Identity
	_, branch, err := runner.OwnedJournalScope(rec)
	if err != nil {
		return err
	}
	if contract.ParentBranch != branch || contract.Workspace == nil {
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
	policy, _ := resolveRecoveryPolicy(r.layout, r.config)
	maxBytes := policy.MaxArchiveBytesEffective()
	path, err := parentArchiveInventoryPath(ctx, r.layout, record)
	if err != nil {
		return err
	}
	var state recovery.RetainedParentState
	if err := r.worktrees.WithRecoveryMirror(ctx, url, func(repository string) error {
		if err := recovery.ImportSnapshotBundle(ctx, repository, path, record, maxBytes); err != nil {
			return err
		}
		var err error
		state, err = recovery.ReadRetainedParentState(ctx, repository, record)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state.Policy, contract.Workspace.Snapshot.Policy) {
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

func (r parentArchiveRestorer) authorize(ctx context.Context, reader *journal.Reader, archive runner.ParentWorkspaceArchive) (recovery.Record, childpod.Contract, error) {
	var record recovery.Record
	var empty childpod.Contract
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
	if err := record.Validate(); err != nil {
		return record, empty, err
	}
	if record.RunID != id.RunID {
		return record, empty, errors.New("parent archive record belongs to another run")
	}
	contract, _, err := readParentArchiveOutput(ctx, reader, archive.Output, record.RepositoryKey)
	if err != nil {
		return record, empty, err
	}
	if !reflect.DeepEqual(contract.ParentOrigin, archive.Custody.Origin) {
		return record, empty, errors.New("parent archive origin differs from retained worker")
	}
	// The receipt pins the exact contract, not just a compatible source policy.
	var preview struct {
		ContractDigest string `json:"contractDigest"`
	}
	data, err = reader.ArtifactBytesBounded(archive.Output, 24<<20)
	if err != nil {
		return record, empty, err
	}
	if json.Unmarshal(data, &preview) != nil || preview.ContractDigest != archive.ContractDigest {
		return record, empty, errors.New("parent archive contract changed")
	}
	return record, contract, nil
}

func parentArchiveInventoryPath(ctx context.Context, layout instance.Layout, record recovery.Record) (string, error) {
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), recovery.MaxInventoryEntries)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Record.Ref != record.Ref || entry.Record.RepositoryKey != record.RepositoryKey {
			continue
		}
		current := entry.Record
		if current.RetainUntil.Before(record.RetainUntil) || !time.Now().Before(current.RetainUntil) {
			return "", errors.New("parent archive retention expired or moved backwards")
		}
		current.RetainUntil = record.RetainUntil
		if current != record {
			return "", errors.New("parent archive inventory record changed")
		}
		return filepath.Join(filepath.Dir(entry.RecordPath), recovery.BundleFileName), nil
	}
	return "", errors.New("parent archive is unavailable in retained inventory")
}
