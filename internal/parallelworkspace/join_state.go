package parallelworkspace

import (
	"encoding/json"
	"errors"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

type joinIntent struct {
	Version  int                               `json:"version"`
	Request  spec.JoinRequest                  `json:"request"`
	Prepared recovery.PreparedChildDisposition `json:"prepared"`
}

type joinReady struct {
	Version int                     `json:"version"`
	Intent  journal.Ref             `json:"intent"`
	Source  spec.Source             `json:"source"`
	Plan    recovery.ChildApplyPlan `json:"plan"`
}

type joinCarrier struct {
	Version  int                    `json:"version"`
	Intent   journal.Ref            `json:"intent"`
	Snapshot recovery.ChildSnapshot `json:"snapshot"`
}

func joinArtifact(rec Recorder, name string, value any) (journal.Ref, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return journal.Ref{}, err
	}
	ref, err := rec.RecordArtifactBoundedWithIntegrity(name, data, apiv1.IntegrityTrusted, spec.MaxJoinMetadataBytes)
	if err == nil && ref.Digest != journal.Digest(data) {
		err = errors.New("parallel join artifact changed during recording")
	}
	return ref, err
}

func joinTransition(rec Recorder, state spec.JoinState, phase string) error {
	if err := rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Parallel: state.Parallel, Runner: map[string]any{"kind": spec.JoinKind, "phase": phase, "join": state.JoinReceipt}}); err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	states, err := spec.ReadJoins(reader)
	if err != nil {
		return err
	}
	actual := states[state.Sequence]
	if actual == nil || actual.JoinReceipt != state.JoinReceipt || actual.Parallel != state.Parallel || actual.Applied != (phase == "applied") {
		return errors.New("parallel join transition was not durably acknowledged")
	}
	return nil
}

func readJoinArtifact(reader *journal.Reader, ref journal.Ref, value any) error {
	if ref.Integrity != apiv1.IntegrityTrusted {
		return errors.New("parallel join artifact lacks host provenance")
	}
	data, err := reader.ArtifactBytesBounded(ref, spec.MaxJoinMetadataBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func readJoinIntent(reader *journal.Reader, state spec.JoinState) (joinIntent, error) {
	var intent joinIntent
	if err := readJoinArtifact(reader, state.Intent, &intent); err != nil {
		return intent, err
	}
	if intent.Version != 1 || intent.Request.Sequence != state.Sequence || intent.Request.Parallel != state.Parallel {
		return intent, errors.New("parallel join intent changed scope")
	}
	return intent, validateJoinRequest(reader, intent.Request)
}

func readJoinReady(reader *journal.Reader, state spec.JoinState, intent joinIntent) (joinReady, recovery.ChildSnapshot, error) {
	var ready joinReady
	var carrier joinCarrier
	if err := readJoinArtifact(reader, state.Ready, &ready); err != nil {
		return ready, carrier.Snapshot, err
	}
	if ready.Version != 1 || ready.Intent != state.Intent || ready.Plan.Version != 1 || ready.Plan.Parent != nil || !reflect.DeepEqual(ready.Plan.Disposition, intent.Prepared) {
		return ready, carrier.Snapshot, errors.New("parallel join application changed prepared disposition")
	}
	if err := readJoinArtifact(reader, ready.Source.Metadata, &carrier); err != nil {
		return ready, carrier.Snapshot, err
	}
	snapshot := carrier.Snapshot
	expected := joinSnapshot(intent.Prepared)
	expected.Record.ArchiveDigest = ready.Source.Bundle.Digest
	expected.Record.ArchiveBytes = ready.Source.Bundle.Size
	expected.Record.ArchiveFormat = "delta"
	if carrier.Version != 1 || carrier.Intent != state.Intent || ready.Source.Bundle.Integrity != apiv1.IntegrityTrusted || snapshot.Record.SnapshotSHA != ready.Source.SnapshotSHA || !reflect.DeepEqual(expected, snapshot) {
		return ready, snapshot, errors.New("parallel join carrier changed exact preparation")
	}
	_, err := reader.ArtifactBytesBounded(ready.Source.Bundle, MaxBundleBytes)
	return ready, snapshot, err
}

func validateJoinRequest(reader *journal.Reader, request spec.JoinRequest) error {
	if err := validateJoinIdentity(reader, request); err != nil {
		return err
	}
	var plan struct {
		Version    int
		RunID      string
		Gaggle     string
		Parallel   string
		Sequence   uint64
		Source     spec.Source
		Root       *worktree.StageCustody
		Workspaces []worktree.StageCustody
	}
	if err := readJoinArtifact(reader, request.Plan, &plan); err != nil {
		return err
	}
	seed, err := readSource(reader, request.Request, request.Seed)
	if err != nil {
		return err
	}
	if plan.Version != 1 || plan.RunID != request.RunID || plan.Gaggle != request.Gaggle || plan.Parallel != request.Parallel || plan.Sequence != request.Sequence || plan.Source != request.Seed || plan.Root == nil || *plan.Root != request.Root || request.Workspace != "" || len(request.Results) != len(plan.Workspaces) || len(request.Results) == 0 || len(request.Results) > 128 || request.Root.OwnerRunID != seed.Record.RunID {
		return errors.New("parallel join differs from reserved root and forks")
	}
	for index, result := range request.Results {
		if result.Branch != index+1 || result.Custody != plan.Workspaces[index] {
			return errors.New("parallel join changed branch declaration order or custody")
		}
		if _, err := ReadResult(reader, joinResultRequest(request, result), result.Source); err != nil {
			return err
		}
	}
	return validateJoinEvidence(reader, request)
}

func joinResultRequest(request spec.JoinRequest, result spec.JoinResult) spec.ResultRequest {
	return spec.ResultRequest{Request: request.Request, Plan: request.Plan, Seed: request.Seed, Branch: result.Branch, Status: result.Status, Custody: result.Custody}
}

func joinSnapshot(prepared recovery.PreparedChildDisposition) recovery.ChildSnapshot {
	return recovery.ChildSnapshot{Record: prepared.Prepared, TreeSHA: prepared.TreeSHA, IndexDigest: prepared.ExpectedParent.IndexDigest, Policy: prepared.ExpectedParent.Policy}
}

func validateJoinIdentity(reader *journal.Reader, request spec.JoinRequest) error {
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.Child != nil || id.RunID != request.RunID || id.Gaggle != request.Gaggle {
		return errors.New("parallel join changed root journal identity")
	}
	return nil
}
