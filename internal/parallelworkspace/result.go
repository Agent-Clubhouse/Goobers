package parallelworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

type resultMetadata struct {
	Version  int                    `json:"version"`
	Plan     journal.Ref            `json:"plan"`
	Seed     spec.Source            `json:"seed"`
	Branch   int                    `json:"branch"`
	Status   journal.BranchStatus   `json:"status"`
	Custody  worktree.StageCustody  `json:"custody"`
	Snapshot recovery.ChildSnapshot `json:"snapshot"`
}

// Result captures a stopped branch once, or imports only its recorded result.
// A replay never substitutes the branch's newer files for acknowledged output.
func (s Service) Result(ctx context.Context, rec Recorder, request spec.ResultRequest, previous *spec.Source) (spec.Source, error) {
	var result spec.Source
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return result, err
	}
	seed, err := ReadSource(reader, request.Seed, request.Parallel, request.Sequence)
	if err != nil {
		return result, err
	}
	if seed.Record.RunID != request.RunID || seed.Record.RepositoryKey != repositoryKey(request.Repository) || !seed.Record.CreatedAt.Equal(request.At) || s.Worktrees == nil || s.CloneURL == nil {
		return result, errors.New("parallel result source owner changed")
	}
	url, err := s.CloneURL(request.Repository)
	if err != nil {
		return result, err
	}
	owner, err := worktree.ParallelForkCustody(worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: request.RunID, Gaggle: request.Gaggle, ParallelSequence: request.Sequence, Branch: request.Branch, SnapshotSHA: request.Seed.SnapshotSHA})
	if err != nil || owner != request.Custody {
		return result, errors.New("parallel result physical owner changed")
	}
	if previous == nil {
		workspace, err := s.Worktrees.AdoptHeldStage(ctx, url, owner)
		if err != nil {
			return result, err
		}
		snapshot, err := recovery.CaptureChildTreeResult(ctx, workspace.Path, request.RunID, seed, request.At, request.At.Add(30*24*time.Hour))
		if err != nil {
			return result, err
		}
		result, err = recordSnapshot(ctx, rec, workspace.Path, "parallel-result", request, snapshot, func(value recovery.ChildSnapshot) any {
			return resultMetadata{Version: 1, Plan: request.Plan, Seed: request.Seed, Branch: request.Branch, Status: request.Status, Custody: owner, Snapshot: value}
		})
		if err != nil {
			return result, err
		}
	} else {
		result = *previous
	}
	snapshot, err := ReadResult(reader, request, result)
	if err != nil {
		return result, err
	}
	return result, s.importSource(ctx, reader, url, result, snapshot)
}

// ReadResult validates a host carrier against its original fork and branch.
func ReadResult(reader *journal.Reader, request spec.ResultRequest, source spec.Source) (recovery.ChildSnapshot, error) {
	var metadata resultMetadata
	if source.Metadata.Integrity != apiv1.IntegrityTrusted || source.Bundle.Integrity != apiv1.IntegrityTrusted || request.Plan.Integrity != apiv1.IntegrityTrusted {
		return metadata.Snapshot, errors.New("parallel result lacks host provenance")
	}
	if _, err := reader.ArtifactBytesBounded(request.Plan, MaxMetadataBytes); err != nil {
		return metadata.Snapshot, err
	}
	seed, err := ReadSource(reader, request.Seed, request.Parallel, request.Sequence)
	if err != nil {
		return metadata.Snapshot, err
	}
	data, err := reader.ArtifactBytesBounded(source.Metadata, MaxMetadataBytes)
	if err != nil {
		return metadata.Snapshot, err
	}
	if json.Unmarshal(data, &metadata) != nil || !metadata.matches(request) {
		return metadata.Snapshot, errors.New("parallel result branch identity changed")
	}
	record := metadata.Snapshot.Record
	if err := record.Validate(); err != nil {
		return metadata.Snapshot, err
	}
	if record.RunID != seed.Record.RunID || record.RepositoryKey != seed.Record.RepositoryKey || record.BaseSHA != seed.Record.SnapshotSHA || record.SnapshotSHA != source.SnapshotSHA || record.ArchiveDigest != source.Bundle.Digest || record.ArchiveBytes != source.Bundle.Size || !record.CreatedAt.Equal(seed.Record.CreatedAt) || !reflect.DeepEqual(metadata.Snapshot.Policy, seed.Policy) {
		return metadata.Snapshot, errors.New("parallel result snapshot identity changed")
	}
	if _, err := reader.ArtifactBytesBounded(source.Bundle, MaxBundleBytes); err != nil {
		return metadata.Snapshot, err
	}
	return metadata.Snapshot, nil
}

func (m resultMetadata) matches(request spec.ResultRequest) bool {
	return m.Version == 1 && m.Plan == request.Plan && m.Seed == request.Seed && m.Branch == request.Branch && m.Status == request.Status && m.Custody == request.Custody
}
