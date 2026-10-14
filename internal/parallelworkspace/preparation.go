package parallelworkspace

import (
	"encoding/json"
	"errors"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
)

const (
	preparationKind         = "isolated.parallel.snapshot.prepared"
	preparationReleasedKind = "isolated.parallel.snapshot.released"
	maxPreparations         = 512
)

// Preparation is host ownership recorded before bundle generation can pin Git
// objects. It is not a complete archive or permission to remove a workspace.
type Preparation struct {
	Version  int                    `json:"version"`
	Request  spec.ResultRequest     `json:"request"`
	Snapshot recovery.ChildSnapshot `json:"snapshot"`
}

// PendingPreparation names an unresolved capture, including captures that
// never reached a fork plan or completed branch result.
type PendingPreparation struct {
	Sequence  uint64
	Reference journal.Ref
	Value     Preparation
}

type preparationRelease struct {
	Sequence  uint64      `json:"preparedAt"`
	Reference journal.Ref `json:"preparation"`
}

// PendingPreparations validates the bounded host ledger before any cleanup.
func PendingPreparations(reader *journal.Reader) ([]PendingPreparation, error) {
	pending, _, err := readPreparations(reader)
	return pending, err
}

func readPreparations(reader *journal.Reader) ([]PendingPreparation, int, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, 0, err
	}
	active := map[uint64]PendingPreparation{}
	total := 0
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["kind"] {
		case preparationKind:
			total++
			if total > maxPreparations || event.Branch != 0 {
				return nil, total, errors.New("parallel capture ownership limit or scope changed")
			}
			value, err := readPreparation(reader, event)
			if err != nil {
				return nil, total, err
			}
			active[event.Seq] = value
		case preparationReleasedKind:
			if err := consumePreparationRelease(event, active); err != nil {
				return nil, total, err
			}
		}
	}
	result := make([]PendingPreparation, 0, len(active))
	for _, value := range active {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result, total, nil
}

func consumePreparationRelease(event journal.Event, active map[uint64]PendingPreparation) error {
	var released preparationRelease
	data, err := json.Marshal(event.Runner["release"])
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &released) != nil || event.Branch != 0 || released.Sequence >= event.Seq {
		return errors.New("invalid parallel capture release")
	}
	previous, ok := active[released.Sequence]
	if !ok || previous.Reference != released.Reference {
		return errors.New("parallel capture release has no exact owner")
	}
	delete(active, released.Sequence)
	return nil
}

func readPreparation(reader *journal.Reader, event journal.Event) (PendingPreparation, error) {
	value := PendingPreparation{Sequence: event.Seq}
	data, err := json.Marshal(event.Runner["preparation"])
	if err != nil || json.Unmarshal(data, &value.Reference) != nil || value.Reference.Integrity != apiv1.IntegrityTrusted {
		return value, errors.New("parallel capture lacks trusted preparation")
	}
	data, err = reader.ArtifactBytesBounded(value.Reference, MaxMetadataBytes)
	if err != nil {
		return value, err
	}
	if json.Unmarshal(data, &value.Value) != nil || value.Value.Version != 1 {
		return value, errors.New("invalid parallel capture preparation")
	}
	if err := validatePreparation(reader, value.Value); err != nil {
		return value, err
	}
	if value.Value.Request.Sequence >= event.Seq || event.Parallel != value.Value.Request.Parallel {
		return value, errors.New("parallel capture boundary changed")
	}
	return value, nil
}

func validatePreparation(reader *journal.Reader, value Preparation) error {
	request, record := value.Request, value.Snapshot.Record
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.Child != nil || request.RunID != id.RunID || request.Gaggle != id.Gaggle || record.RunID != id.RunID || record.RepositoryKey != repositoryKey(request.Repository) || request.Workspace != "" || !record.CreatedAt.Equal(request.At) {
		return errors.New("parallel capture owner changed")
	}
	if err := validatePreparedSnapshot(value.Snapshot); err != nil {
		return err
	}
	if err := verifySourceBoundary(reader, sourceMetadata{Gaggle: id.Gaggle, Parallel: request.Parallel, Sequence: request.Sequence, Snapshot: value.Snapshot}); err != nil {
		return err
	}
	if request.Branch < 0 || request.Branch > 128 {
		return errors.New("parallel preparation branch outside bound")
	}
	if request.Join {
		return validateJoinPreparation(reader, value)
	}
	if request.Branch != 0 {
		if request.Plan.Integrity != apiv1.IntegrityTrusted || record.BaseSHA != request.Seed.SnapshotSHA || request.Custody.OwnerRunID != id.RunID {
			return errors.New("parallel result preparation changed fork ownership")
		}
		if _, err := reader.ArtifactBytesBounded(request.Plan, MaxMetadataBytes); err != nil {
			return err
		}
		if _, err := ReadSource(reader, request.Seed, request.Parallel, request.Sequence); err != nil {
			return err
		}
	}
	return nil
}

func recordPreparation(rec Recorder, request spec.ResultRequest, snapshot recovery.ChildSnapshot) error {
	request.Workspace = ""
	value := Preparation{Version: 1, Request: request, Snapshot: snapshot}
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	if err := validatePreparation(reader, value); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	pending, total, err := readPreparations(reader)
	if err != nil {
		return err
	}
	for _, existing := range pending {
		if existing.Reference.Digest == journal.Digest(data) {
			return nil
		}
	}
	if total >= maxPreparations {
		return errors.New("parallel capture preparation limit reached")
	}
	ref, err := rec.RecordArtifactBoundedWithIntegrity("parallel-snapshot-preparation.json", data, apiv1.IntegrityTrusted, MaxMetadataBytes)
	if err != nil {
		return err
	}
	if ref.Digest != journal.Digest(data) {
		return errors.New("parallel preparation changed during recording")
	}
	if err := rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: request.Parallel, Runner: map[string]any{"kind": preparationKind, "preparation": ref}}); err != nil {
		return err
	}
	pending, err = PendingPreparations(reader)
	if err != nil {
		return err
	}
	for _, value := range pending {
		if value.Reference == ref {
			return nil
		}
	}
	return errors.New("parallel capture preparation was not durably acknowledged")
}

// ReleasePreparation follows archive transfer or verified deletion under the
// terminal journal and repository owners. An identical retry has no new effect.
func ReleasePreparation(run *journal.Run, expected PendingPreparation) error {
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	pending, err := PendingPreparations(reader)
	if err != nil {
		return err
	}
	for _, value := range pending {
		if value.Sequence != expected.Sequence {
			continue
		}
		if value.Reference != expected.Reference {
			return errors.New("parallel capture release identity changed")
		}
		return run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: value.Value.Request.Parallel, Runner: map[string]any{"kind": preparationReleasedKind, "release": preparationRelease{Sequence: value.Sequence, Reference: value.Reference}}})
	}
	return nil
}

func validatePreparedSnapshot(snapshot recovery.ChildSnapshot) error {
	record := snapshot.Record
	if err := record.ValidateRestorable(); err != nil {
		return err
	}
	if record.ArchiveDigest != "" || record.ArchiveBytes != 0 || record.ArchiveFormat != "" {
		return errors.New("parallel preparation must precede archive publication")
	}
	ref, err := recovery.RefForSnapshot(record.RunID, record.SnapshotSHA)
	if err != nil || ref != record.Ref {
		return errors.New("parallel preparation requires exact snapshot reference")
	}
	if err := snapshot.Policy.Validate(); err != nil {
		return err
	}
	return nil
}
