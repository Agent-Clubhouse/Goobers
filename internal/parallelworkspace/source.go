// Package parallelworkspace owns host snapshot capture for durable branch forks.
package parallelworkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// MaxBundleBytes bounds a fork source carrier.
const MaxBundleBytes = 16 << 20

// MaxMetadataBytes bounds source identity and exclusion policy.
const MaxMetadataBytes = 128 << 10

// Recorder is supplied only by the host execution owner.
type Recorder interface {
	Dir() string
	RecordArtifactBoundedWithIntegrity(string, []byte, apiv1.Integrity, int) (journal.Ref, error)
}

// Service captures and imports fork sources through a configured managed mirror.
type Service struct {
	Worktrees *worktree.Manager
	CloneURL  func(apiv1.RepoRef) (string, error)
	Policy    func(string) (recovery.SnapshotPolicy, error)
}

type sourceMetadata struct {
	Version  int                    `json:"version"`
	Gaggle   string                 `json:"gaggle"`
	Parallel string                 `json:"parallel"`
	Sequence uint64                 `json:"sequence"`
	Snapshot recovery.ChildSnapshot `json:"snapshot"`
}

// Prepare captures once, or verifies and imports a previously recorded source.
// Recovery never reads the live root workspace and never fetches remote data.
func (s Service) Prepare(ctx context.Context, rec Recorder, request spec.Request, previous *spec.Source) (spec.Source, error) {
	var source spec.Source
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return source, err
	}
	id, err := reader.Identity()
	if err != nil {
		return source, err
	}
	if id.RunID != request.RunID || id.Gaggle != request.Gaggle || id.Child != nil || request.Sequence == 0 || request.At.IsZero() || s.Worktrees == nil || s.CloneURL == nil {
		return source, errors.New("parallel source lacks exact parent ownership")
	}
	url, err := s.CloneURL(request.Repository)
	if err != nil {
		return source, err
	}
	if previous == nil {
		source, err = s.capture(ctx, rec, request)
		if err != nil {
			return source, err
		}
	} else {
		source = *previous
	}
	snapshot, err := readSource(reader, request, source)
	if err != nil {
		return source, err
	}
	err = s.importSource(ctx, reader, url, source, snapshot)
	return source, err
}

func (s Service) capture(ctx context.Context, rec Recorder, request spec.Request) (spec.Source, error) {
	var source spec.Source
	if request.Workspace == "" || s.Policy == nil {
		return source, errors.New("parallel source workspace policy unavailable")
	}
	policy, err := s.Policy(request.Workspace)
	if err != nil {
		return source, err
	}
	snapshot, err := recovery.CaptureChildSnapshot(ctx, request.Workspace, repositoryKey(request.Repository), request.RunID, request.At, request.At.Add(30*24*time.Hour), policy)
	if err != nil {
		return source, err
	}
	var data bytes.Buffer
	snapshot, err = recovery.WriteChildSnapshotBundle(ctx, request.Workspace, snapshot, &data, MaxBundleBytes)
	if err != nil {
		return source, err
	}
	source.Bundle, err = rec.RecordArtifactBoundedWithIntegrity("parallel-source.bundle", data.Bytes(), apiv1.IntegrityTrusted, MaxBundleBytes)
	if err != nil {
		return source, err
	}
	if source.Bundle.Digest != snapshot.Record.ArchiveDigest || source.Bundle.Size != snapshot.Record.ArchiveBytes {
		return source, errors.New("parallel source changed during recording")
	}
	source.SnapshotSHA = snapshot.Record.SnapshotSHA
	metadata := sourceMetadata{Version: 1, Gaggle: request.Gaggle, Parallel: request.Parallel, Sequence: request.Sequence, Snapshot: snapshot}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return source, err
	}
	source.Metadata, err = rec.RecordArtifactBoundedWithIntegrity("parallel-source.json", encoded, apiv1.IntegrityTrusted, MaxMetadataBytes)
	if err == nil && source.Metadata.Digest != journal.Digest(encoded) {
		err = errors.New("parallel source metadata changed during recording")
	}
	return source, err
}

func readSource(reader *journal.Reader, request spec.Request, source spec.Source) (recovery.ChildSnapshot, error) {
	snapshot, err := ReadSource(reader, source, request.Parallel, request.Sequence)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Record.RunID != request.RunID || snapshot.Record.RepositoryKey != repositoryKey(request.Repository) || !snapshot.Record.CreatedAt.Equal(request.At) {
		return snapshot, errors.New("parallel source identity changed")
	}
	return snapshot, nil
}

// ReadSource verifies host metadata against the journal's immutable identity and
// original root boundary. It supplies archive policy without inventing a worker
// contract and does not mutate or acquire a repository.
func ReadSource(reader *journal.Reader, source spec.Source, parallel string, sequence uint64) (recovery.ChildSnapshot, error) {
	var metadata sourceMetadata
	if source.Metadata.Integrity != apiv1.IntegrityTrusted || source.Bundle.Integrity != apiv1.IntegrityTrusted {
		return metadata.Snapshot, errors.New("parallel source lacks host provenance")
	}
	data, err := reader.ArtifactBytesBounded(source.Metadata, MaxMetadataBytes)
	if err != nil {
		return metadata.Snapshot, err
	}
	if json.Unmarshal(data, &metadata) != nil || metadata.Version != 1 || metadata.Parallel != parallel || metadata.Sequence != sequence {
		return metadata.Snapshot, errors.New("parallel source scope changed")
	}
	id, err := reader.Identity()
	if err != nil {
		return metadata.Snapshot, err
	}
	record := metadata.Snapshot.Record
	if id.Child != nil || record.RunID != id.RunID || metadata.Gaggle != id.Gaggle || record.SnapshotSHA != source.SnapshotSHA || record.ArchiveDigest != source.Bundle.Digest || record.ArchiveBytes != source.Bundle.Size {
		return metadata.Snapshot, errors.New("parallel source identity changed")
	}
	if err := record.Validate(); err != nil {
		return metadata.Snapshot, err
	}
	if err := metadata.Snapshot.Policy.Validate(); err != nil {
		return metadata.Snapshot, err
	}
	if err := verifySourceBoundary(reader, metadata); err != nil {
		return metadata.Snapshot, err
	}
	if _, err := reader.ArtifactBytesBounded(source.Bundle, MaxBundleBytes); err != nil {
		return metadata.Snapshot, err
	}
	return metadata.Snapshot, nil
}

func verifySourceBoundary(reader *journal.Reader, metadata sourceMetadata) error {
	events, err := reader.Events()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Seq != metadata.Sequence {
			continue
		}
		if event.Type != journal.EventParallelStarted || event.Branch != 0 || event.Parallel != metadata.Parallel || !event.Time.Equal(metadata.Snapshot.Record.CreatedAt) {
			return errors.New("parallel source boundary changed")
		}
		return nil
	}
	return errors.New("parallel source boundary missing")
}

func repositoryKey(ref apiv1.RepoRef) string {
	return (providers.RepositoryRef{Provider: providers.ProviderKind(ref.Provider), URL: ref.BaseURL, Owner: ref.Owner, Project: ref.Project, Name: ref.Name}).CanonicalKey()
}

func (s Service) importSource(ctx context.Context, reader *journal.Reader, url string, source spec.Source, snapshot recovery.ChildSnapshot) error {
	data, err := reader.ArtifactBytesBounded(source.Bundle, MaxBundleBytes)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "goobers-parallel-source-*.bundle")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	found, err := s.Worktrees.WithExistingMirror(ctx, url, func(mirror string) error {
		return recovery.ImportChildSnapshot(ctx, mirror, file.Name(), snapshot, MaxBundleBytes)
	})
	if err == nil && !found {
		err = errors.New("parallel source mirror unavailable")
	}
	return err
}
