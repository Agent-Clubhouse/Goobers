package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// parentKitWriter reuses ordinary kit composition under exact retained stage
// authority. Provider permissions remain delegation metadata, never pod secrets.
type parentKitWriter struct {
	service  *daemonCredentialService
	identity journal.RunIdentity
	blobs    blobstore.Store
	recorder runner.ArtifactRecorder
}

func (w parentKitWriter) WriteKit(ctx context.Context, attempt dispatcher.Attempt, ceiling credentials.ChildCeiling) (string, error) {
	if err := w.validateCustody(attempt); err != nil {
		return "", err
	}
	if err := verifyChildKitIdentity(w.identity, attempt); err != nil {
		return "", err
	}
	owned, branch, err := runner.OwnedJournalScope(w.recorder)
	if err != nil {
		return "", err
	}
	reader, err := journal.OpenReadOnly(owned.Dir())
	if err != nil {
		return "", err
	}
	if attempt.Envelope.ChildWorkflowOrigin == nil {
		return "", childworkflow.ErrAuthorityUnavailable
	}
	id, started, err := childworkflow.VerifyActiveStage(ctx, reader, w.identity.RunID, *attempt.Envelope.ChildWorkflowOrigin)
	if err != nil || !reflect.DeepEqual(id, w.identity) || started.Seq != uint64(attempt.PodAttempt) || started.Branch != branch {
		return "", errors.Join(childworkflow.ErrAuthorityUnavailable, err)
	}
	lease, err := w.service.children.AcquirePreparedStage(ctx, *attempt.Envelope)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	snapshot, release, err := w.snapshot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	kit, err := (agenticKitWriter{registrar: w.service.shared}).buildSnapshotKit(w.service.layout, snapshot, *attempt.Envelope, kitModeFor(attempt))
	if err != nil {
		return "", err
	}
	if err := narrowChildKit(kit, ceiling); err != nil {
		return "", err
	}
	if err := childworkflow.AttachParentAuthoringSkill(kit, lease.Authority); err != nil {
		return "", err
	}
	kit.Envelope.Workspace = ""
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		return "", err
	}
	if len(data) > maxChildKitBytes {
		return "", errors.New("parent kit exceeds 8 MiB custody bound")
	}
	if err := lease.Verify(ctx); err != nil {
		return "", err
	}
	ref, err := owned.RecordArtifact(fmt.Sprintf("parent-kit/%s-%d-%d.json", attempt.Stage, attempt.Number, attempt.PodAttempt), data)
	if err != nil {
		return "", err
	}
	if ref.Digest != digest {
		return "", errors.New("parent kit changed during journal custody")
	}
	if err := w.blobs.Put(ctx, digest, data); err != nil {
		return "", err
	}
	if err := lease.Verify(ctx); err != nil {
		return "", err
	}
	return digest, nil
}

func (w parentKitWriter) snapshot(ctx context.Context) (*workerConfigSnapshot, func(), error) {
	store, err := executionGenerationStore(w.service.layout)
	if err != nil {
		return nil, nil, err
	}
	directory, lease, err := store.Acquire(ctx, w.identity.ConfigGeneration)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = lease.Release() }
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("parent kit: load retained config: %w (%s)", err, validationIssueSummary(report))
	}
	instructions, skills, err := loadSnapshotGooberInputs(directory, set)
	if err != nil {
		release()
		return nil, nil, err
	}
	snapshot := &workerConfigSnapshot{configDir: directory, generation: w.identity.ConfigGeneration, cfg: w.service.config, set: set, instructions: instructions, skillPackages: skills, digests: newGooberDigestIndex(w.service.config, set, instructions, skills)}
	digest, err := snapshot.gooberDigestFor(w.identity.Gaggle, w.identity.Workflow)
	if err != nil || digest != w.identity.GooberDigest {
		release()
		return nil, nil, errors.Join(errors.New("parent Goober pin differs from retained source"), err)
	}
	return snapshot, release, nil
}

func (w parentKitWriter) validateCustody(attempt dispatcher.Attempt) error {
	if w.service == nil || w.service.children == nil || w.service.shared == nil || w.blobs == nil || w.recorder == nil || attempt.Envelope == nil || !attempt.WorkflowParent || attempt.Review || w.identity.Child != nil {
		return errors.New("parent kit custody unavailable")
	}
	return nil
}
