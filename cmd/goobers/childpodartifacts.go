package main

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

func (p *childStagePod) workspace(ctx context.Context, reader *journal.Reader, env apiv1.InvocationEnvelope, mode apiv1.WorkspaceMode) (*childpod.WorkspaceInput, error) {
	if mode == apiv1.WorkspaceScratch {
		return nil, nil
	}
	if !mode.IsRepoBacked() || p.runtime.worktrees == nil {
		return nil, errors.New("child stage workspace is unsupported")
	}
	admission, err := runner.PinnedChildWorkspaceAdmission(reader, p.identity)
	if err != nil {
		return nil, err
	}
	if admission == nil {
		return nil, errors.New("child workspace admission missing")
	}
	url, err := childRepoCloneURL(p.runtime.repoRef)
	if err != nil {
		return nil, err
	}
	fork, err := (&childworkflow.WorkspaceCoordinator{Queue: p.service.childQueue}).ExecutionFork(ctx, p.start.Child, p.identity.RunID, url)
	if err != nil {
		return nil, err
	}
	if fork.Record.SnapshotSHA != admission.ForkSHA || fork.Record.RepositoryKey != childRepoKey(p.runtime.repoRef) || admission.RepositoryDigest != worktree.RepositoryDigest(url) {
		return nil, errors.New("child workspace fork differs from retained custody")
	}
	owned, err := p.runtime.worktrees.AdoptChildFromSnapshot(ctx, worktree.ChildOptions{RepoURL: url, RunID: admission.WorkspaceID, OwnerRunID: p.identity.RunID, Gaggle: p.identity.Gaggle, SnapshotSHA: admission.ForkSHA})
	if err != nil {
		return nil, err
	}
	if owned.Path != env.Workspace {
		return nil, errors.New("child invocation workspace differs from managed custody")
	}
	return &childpod.WorkspaceInput{Path: owned.Path, Fork: fork}, nil
}

func copyContainedPodContext(ctx context.Context, reader *journal.Reader, runID string, blobs blobstore.Store, pointers []apiv1.ContextPointer) error {
	if len(pointers) > 128 {
		return errors.New("child context exceeds pointer bound")
	}
	for _, pointer := range pointers {
		if pointer.Artifact == nil {
			continue
		}
		if pointer.RunID != "" && pointer.RunID != runID {
			return errors.New("child context requires same-run artifact custody")
		}
		ref := pointer.Artifact
		if err := ref.Validate(); err != nil {
			return err
		}
		data, err := reader.ArtifactBytesBounded(journal.Ref{Path: ref.Path, Digest: ref.Digest, Size: ref.Size}, triggerqueue.MaxChildBlobBytes)
		if err != nil {
			return err
		}
		if err = blobs.Put(ctx, ref.Digest, data); err != nil {
			return err
		}
	}
	return nil
}

func adoptContainedPodOutputs(ctx context.Context, recorder runner.ArtifactRecorder, blobs blobstore.Store, out *dispatcher.SurrenderedResult) error {
	if out.Result.Integrity != "" && !out.Result.Integrity.Valid() {
		return errors.New("child output has invalid integrity")
	}
	out.Result.Integrity = containedOutputIntegrity(out.Result.Integrity)
	if len(out.Mutations) != 0 || len(out.MutationIssues) != 0 || out.Result.WorkspaceRevision != nil {
		return errors.New("child surrendered unsupported provider or revision control effects")
	}
	if len(out.Result.Artifacts) > 128 {
		return errors.New("child output exceeds artifact pointer bound")
	}
	for i := range out.Result.Artifacts {
		if err := adoptContainedPodArtifact(ctx, recorder, blobs, &out.Result.Artifacts[i]); err != nil {
			return err
		}
	}
	if out.Result.Transcript != nil {
		if err := adoptContainedPodArtifact(ctx, recorder, blobs, out.Result.Transcript); err != nil {
			return err
		}
	}
	if out.Verdict != nil {
		if len(out.Verdict.Evidence) > 128 {
			return errors.New("child verdict exceeds evidence pointer bound")
		}
		for i := range out.Verdict.Evidence {
			if err := adoptContainedPodArtifact(ctx, recorder, blobs, &out.Verdict.Evidence[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func adoptContainedPodArtifact(ctx context.Context, recorder runner.ArtifactRecorder, blobs blobstore.Store, pointer *apiv1.ArtifactPointer) error {
	if err := pointer.Validate(); err != nil {
		return err
	}
	bounded, ok := blobs.(blobstore.BoundedReader)
	if !ok {
		return errors.New("contained output requires bounded blob custody")
	}
	data, err := bounded.GetBounded(ctx, pointer.Digest, triggerqueue.MaxChildBlobBytes)
	if err != nil {
		return err
	}
	if int64(len(data)) != pointer.Size {
		return errors.New("child output size differs from declared pointer")
	}
	pointer.Integrity = containedOutputIntegrity(pointer.Integrity)
	graded, ok := recorder.(interface {
		RecordArtifactWithIntegrity(string, []byte, apiv1.Integrity) (journal.Ref, error)
	})
	if !ok {
		return errors.New("contained output requires provenance-aware recorder")
	}
	ref, err := graded.RecordArtifactWithIntegrity("contained-outputs/"+pointer.Digest[7:], data, pointer.Integrity)
	if err != nil {
		return err
	}
	if ref.Digest != pointer.Digest {
		return fmt.Errorf("child output changed during journal custody")
	}
	pointer.Path, pointer.Size = ref.Path, ref.Size
	return nil
}

// Pod-authored data cannot acquire source/config provenance at the host boundary.
func containedOutputIntegrity(grade apiv1.Integrity) apiv1.Integrity {
	if grade == apiv1.IntegrityUnapproved {
		return grade
	}
	return apiv1.IntegrityDerived
}

func childRepoCloneURL(ref apiv1.RepoRef) (string, error) {
	if repoCloneURL != nil {
		return repoCloneURL(ref)
	}
	return runner.DefaultRepoCloneURL(ref)
}
