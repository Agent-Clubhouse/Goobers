package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func (p *parentStagePod) snapshot(ctx context.Context) (*workerConfigSnapshot, func(), error) {
	store, err := executionGenerationStore(p.service.layout)
	if err != nil {
		return nil, nil, err
	}
	directory, lease, err := store.Acquire(ctx, p.identity.ConfigGeneration)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = lease.Release() }
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		release()
		return nil, nil, err
	}
	instructions, skills, err := loadSnapshotGooberInputs(directory, set)
	if err != nil {
		release()
		return nil, nil, err
	}
	snapshot := &workerConfigSnapshot{configDir: directory, generation: p.identity.ConfigGeneration, cfg: p.service.config, set: set, instructions: instructions, skillPackages: skills, digests: newGooberDigestIndex(p.service.config, set, instructions, skills)}
	digest, err := snapshot.gooberDigestFor(p.identity.Gaggle, p.identity.Workflow)
	if err != nil || digest != p.identity.GooberDigest {
		release()
		return nil, nil, errors.Join(errors.New("parent Goober pin differs from retained source"), err)
	}
	return snapshot, release, nil
}

func writeParentKit(ctx context.Context, service *daemonCredentialService, snapshot *workerConfigSnapshot, attempt dispatcher.Attempt, ceiling credentials.ChildCeiling, blobs blobstore.Store, recorder runner.ArtifactRecorder) (string, error) {
	kit, err := (agenticKitWriter{registrar: service.shared}).buildSnapshotKit(service.layout, snapshot, *attempt.Envelope, kitModeFor(attempt))
	if err != nil {
		return "", err
	}
	if err = narrowChildKit(kit, ceiling); err != nil {
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
	ref, err := recorder.RecordArtifact(fmt.Sprintf("parent-kit/%s-%d-%d.json", attempt.Stage, attempt.Number, attempt.PodAttempt), data)
	if err != nil {
		return "", err
	}
	if ref.Digest != digest {
		return "", errors.New("parent kit changed during journal custody")
	}
	if err = blobs.Put(ctx, digest, data); err != nil {
		return "", err
	}
	return digest, nil
}

func parentJournal(rec runner.ArtifactRecorder) (*journal.Run, journal.RunIdentity, error) {
	jr, ok := rec.(*journal.Run)
	if !ok {
		return nil, journal.RunIdentity{}, errors.New("contained parent requires its owned journal writer")
	}
	reader, err := journal.OpenReadOnly(jr.Dir())
	if err != nil {
		return nil, journal.RunIdentity{}, err
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, id, err
	}
	if id.Child != nil {
		return nil, id, errors.New("generated child cannot enter parent executor")
	}
	return jr, id, nil
}
